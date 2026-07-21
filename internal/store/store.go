// Package store is the persistence layer: SQLite (WAL) with an in-memory
// provider cache on the hot path, and AES-256-GCM encryption for API keys.
package store

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

type Provider struct {
	ID           int64  `json:"id"`
	Name         string `json:"name"`
	Type         string `json:"type"` // openai | anthropic | ollama
	BaseURL      string `json:"base_url"`
	APIKey       string `json:"api_key,omitempty"` // decrypted in memory; never persisted plain
	HasKey       bool   `json:"has_key"`
	DefaultModel string `json:"default_model"`
	Priority     int    `json:"priority"`
	Enabled      bool   `json:"enabled"`
	DocURL       string `json:"doc_url"`
	DocMarkdown  string `json:"doc_markdown,omitempty"`
	InjectDocs   bool   `json:"inject_docs"`
	Models       string `json:"models"`        // comma-separated aliases this provider serves
	Fallback     string `json:"fallback"`      // provider/model to fail over to on transient errors
	PinnedModels string `json:"pinned_models"` // comma-separated curated subset shown to pickers
	ModelsFilter string `json:"models_filter"` // comma globs applied to scans; "!" prefix excludes
}

type Transform struct {
	ID         int64           `json:"id"`
	Name       string          `json:"name"`
	ProviderID int64           `json:"provider_id"` // 0 = all providers
	Target     string          `json:"target"`      // inbound dialect filter: openai|anthropic|ollama|"" = any
	Phase      string          `json:"phase"`       // request | response
	Rules      json.RawMessage `json:"rules"`
	Enabled    bool            `json:"enabled"`
}

type Trace struct {
	ID        int64  `json:"id"`
	TS        int64  `json:"ts"`
	Provider  string `json:"provider"`
	Model     string `json:"model"`
	Inbound   string `json:"inbound"`
	Stream    bool   `json:"stream"`
	Status    int    `json:"status"`
	LatencyMS int64  `json:"latency_ms"`
	Err       string `json:"err"`
	Note      string `json:"note"`
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	CachedTokens     int `json:"cached_tokens"`
	ReqSnip   string `json:"req_snippet"`
	RespSnip  string `json:"resp_snippet"`
}

type Store struct {
	db  *sql.DB
	key []byte // AES-256 key for API-key encryption

	mu          sync.RWMutex
	cache       []Provider // decrypted, sorted by priority — the hot-path registry
	dataVersion int64      // SQLite data_version at last reload; detects writes from other processes
}

var ValidTypes = map[string]bool{"openai": true, "anthropic": true, "ollama": true}

func Open(dataDir string) (*Store, error) {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, err
	}
	key, err := loadOrCreateKey(filepath.Join(dataDir, "secret.key"))
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", filepath.Join(dataDir, "cfrproxy.db")+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // modernc sqlite: single writer, avoids SQLITE_BUSY
	s := &Store{db: db, key: key}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	if err := s.reload(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	_, err := s.db.Exec(`
CREATE TABLE IF NOT EXISTS providers (
  id INTEGER PRIMARY KEY,
  name TEXT UNIQUE NOT NULL,
  type TEXT NOT NULL,
  base_url TEXT NOT NULL,
  api_key_enc BLOB,
  default_model TEXT NOT NULL DEFAULT '',
  priority INTEGER NOT NULL DEFAULT 1000,
  enabled INTEGER NOT NULL DEFAULT 1,
  doc_url TEXT NOT NULL DEFAULT '',
  doc_markdown TEXT NOT NULL DEFAULT '',
  inject_docs INTEGER NOT NULL DEFAULT 0,
  models TEXT NOT NULL DEFAULT '',
  fallback TEXT NOT NULL DEFAULT '',
  pinned_models TEXT NOT NULL DEFAULT '',
  models_filter TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS transforms (
  id INTEGER PRIMARY KEY,
  name TEXT UNIQUE NOT NULL,
  provider_id INTEGER NOT NULL DEFAULT 0,
  target TEXT NOT NULL DEFAULT '',
  phase TEXT NOT NULL,
  rules TEXT NOT NULL,
  enabled INTEGER NOT NULL DEFAULT 1
);
CREATE TABLE IF NOT EXISTS traces (
  id INTEGER PRIMARY KEY,
  ts INTEGER NOT NULL,
  provider TEXT NOT NULL DEFAULT '',
  model TEXT NOT NULL DEFAULT '',
  inbound TEXT NOT NULL DEFAULT '',
  stream INTEGER NOT NULL DEFAULT 0,
  status INTEGER NOT NULL DEFAULT 0,
  latency_ms INTEGER NOT NULL DEFAULT 0,
  err TEXT NOT NULL DEFAULT '',
  note TEXT NOT NULL DEFAULT '',
  prompt_tokens INTEGER NOT NULL DEFAULT 0,
  completion_tokens INTEGER NOT NULL DEFAULT 0,
  cached_tokens INTEGER NOT NULL DEFAULT 0,
  req_snippet TEXT NOT NULL DEFAULT '',
  resp_snippet TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS traces_ts ON traces(ts);
CREATE TABLE IF NOT EXISTS settings (k TEXT PRIMARY KEY, v TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS roundtable_logs (
  id INTEGER PRIMARY KEY,
  ts INTEGER NOT NULL,
  question TEXT NOT NULL DEFAULT '',
  profiles TEXT NOT NULL DEFAULT '',
  rounds INTEGER NOT NULL DEFAULT 0,
  compressed INTEGER NOT NULL DEFAULT 0,
  moderator TEXT NOT NULL DEFAULT '',
  latency_ms INTEGER NOT NULL DEFAULT 0,
  prompt_tokens INTEGER NOT NULL DEFAULT 0,
  completion_tokens INTEGER NOT NULL DEFAULT 0,
  output TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS roundtable_ts ON roundtable_logs(ts);
CREATE TABLE IF NOT EXISTS agent_profiles (
  id INTEGER PRIMARY KEY,
  name TEXT UNIQUE NOT NULL,
  model TEXT NOT NULL,
  persona TEXT NOT NULL DEFAULT '',
  temperature TEXT NOT NULL DEFAULT '',
  enabled INTEGER NOT NULL DEFAULT 1
);
`)
	if err != nil {
		return err
	}
	// additive migrations; duplicate-column errors are fine
	s.db.Exec(`ALTER TABLE providers ADD COLUMN fallback TEXT NOT NULL DEFAULT ''`)
	s.db.Exec(`ALTER TABLE providers ADD COLUMN pinned_models TEXT NOT NULL DEFAULT ''`)
	s.db.Exec(`ALTER TABLE providers ADD COLUMN models_filter TEXT NOT NULL DEFAULT ''`)
	s.db.Exec(`ALTER TABLE traces ADD COLUMN note TEXT NOT NULL DEFAULT ''`)
	s.db.Exec(`ALTER TABLE traces ADD COLUMN prompt_tokens INTEGER NOT NULL DEFAULT 0`)
	s.db.Exec(`ALTER TABLE traces ADD COLUMN completion_tokens INTEGER NOT NULL DEFAULT 0`)
	s.db.Exec(`ALTER TABLE traces ADD COLUMN cached_tokens INTEGER NOT NULL DEFAULT 0`)
	return nil
}

// ---- crypto ----

func loadOrCreateKey(path string) ([]byte, error) {
	if b, err := os.ReadFile(path); err == nil {
		if len(b) != 32 {
			return nil, fmt.Errorf("secret.key: expected 32 bytes, got %d", len(b))
		}
		return b, nil
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, key, 0o600); err != nil {
		return nil, err
	}
	return key, nil
}

func (s *Store) encrypt(plain string) ([]byte, error) {
	if plain == "" {
		return nil, nil
	}
	block, err := aes.NewCipher(s.key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return gcm.Seal(nonce, nonce, []byte(plain), nil), nil
}

func (s *Store) decrypt(blob []byte) (string, error) {
	if len(blob) == 0 {
		return "", nil
	}
	block, err := aes.NewCipher(s.key)
	if err != nil {
		return "", err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", err
	}
	if len(blob) < gcm.NonceSize() {
		return "", errors.New("ciphertext too short")
	}
	plain, err := gcm.Open(nil, blob[:gcm.NonceSize()], blob[gcm.NonceSize():], nil)
	if err != nil {
		return "", err
	}
	return string(plain), nil
}

// ---- provider registry ----

func (s *Store) reload() error {
	rows, err := s.db.Query(`SELECT id,name,type,base_url,api_key_enc,default_model,priority,enabled,doc_url,doc_markdown,inject_docs,models,fallback,pinned_models,models_filter FROM providers`)
	if err != nil {
		return err
	}
	defer rows.Close()
	var out []Provider
	for rows.Next() {
		var p Provider
		var enc []byte
		var enabled, inject int
		if err := rows.Scan(&p.ID, &p.Name, &p.Type, &p.BaseURL, &enc, &p.DefaultModel, &p.Priority, &enabled, &p.DocURL, &p.DocMarkdown, &inject, &p.Models, &p.Fallback, &p.PinnedModels, &p.ModelsFilter); err != nil {
			return err
		}
		p.Enabled, p.InjectDocs = enabled == 1, inject == 1
		if p.APIKey, err = s.decrypt(enc); err != nil {
			return fmt.Errorf("provider %s: %w", p.Name, err)
		}
		p.HasKey = p.APIKey != ""
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Priority < out[j].Priority })
	s.mu.Lock()
	s.cache = out
	s.mu.Unlock()
	return rows.Err()
}

// Providers returns the cached registry sorted by priority. Copies are cheap;
// callers must not mutate returned slices' DocMarkdown in place.
func (s *Store) Providers() []Provider {
	s.maybeReload()
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Provider, len(s.cache))
	copy(out, s.cache)
	return out
}

// maybeReload refreshes the cache when another process (CLI vs running
// server) has written to the DB. data_version only moves for changes made by
// other connections, so this is a no-op for our own writes (which call
// reload() directly).
func (s *Store) maybeReload() {
	var v int64
	if err := s.db.QueryRow(`PRAGMA data_version`).Scan(&v); err != nil {
		return
	}
	s.mu.RLock()
	stale := v != s.dataVersion
	s.mu.RUnlock()
	if stale {
		s.reload()
		s.mu.Lock()
		s.dataVersion = v
		s.mu.Unlock()
	}
}

func (s *Store) ProviderByName(name string) (Provider, bool) {
	for _, p := range s.Providers() {
		if p.Name == name {
			return p, true
		}
	}
	return Provider{}, false
}

func (s *Store) ProviderByID(id int64) (Provider, bool) {
	for _, p := range s.Providers() {
		if p.ID == id {
			return p, true
		}
	}
	return Provider{}, false
}

// Resolve picks a provider for a model string. "provider/model" targets by
// name; otherwise a provider whose models list contains the alias; otherwise
// the highest-priority enabled provider (the active route).
func (s *Store) Resolve(model string) (Provider, string, error) {
	provs := s.Providers()
	if i := strings.IndexByte(model, '/'); i > 0 {
		name, rest := model[:i], model[i+1:]
		for _, p := range provs {
			if p.Name == name && p.Enabled {
				if rest == "" {
					rest = p.DefaultModel
				}
				return p, rest, nil
			}
		}
	}
	for _, p := range provs {
		if !p.Enabled {
			continue
		}
		for _, alias := range strings.Split(p.Models, ",") {
			if strings.TrimSpace(alias) == model && model != "" {
				return p, model, nil
			}
		}
	}
	for _, p := range provs {
		if p.Enabled {
			m := model
			if m == "" || m == "default" {
				m = p.DefaultModel
			}
			return p, m, nil
		}
	}
	return Provider{}, "", errors.New("no enabled providers configured")
}

func (s *Store) SaveProvider(p *Provider) error {
	if !ValidTypes[p.Type] {
		return fmt.Errorf("invalid provider type %q (want openai|anthropic|ollama)", p.Type)
	}
	if p.Name == "" || p.BaseURL == "" {
		return errors.New("name and base_url are required")
	}
	enc, err := s.encrypt(p.APIKey)
	if err != nil {
		return err
	}
	if p.ID == 0 {
		if p.Priority == 0 {
			p.Priority = int(time.Now().Unix() % 1000000) // append at end
		}
		res, err := s.db.Exec(`INSERT INTO providers(name,type,base_url,api_key_enc,default_model,priority,enabled,doc_url,doc_markdown,inject_docs,models,fallback,pinned_models,models_filter) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			p.Name, p.Type, p.BaseURL, enc, p.DefaultModel, p.Priority, b2i(p.Enabled), p.DocURL, p.DocMarkdown, b2i(p.InjectDocs), p.Models, p.Fallback, p.PinnedModels, p.ModelsFilter)
		if err != nil {
			return err
		}
		p.ID, _ = res.LastInsertId()
	} else {
		// empty APIKey on update = keep existing key
		if p.APIKey == "" {
			_, err = s.db.Exec(`UPDATE providers SET name=?,type=?,base_url=?,default_model=?,priority=?,enabled=?,doc_url=?,doc_markdown=?,inject_docs=?,models=?,fallback=?,pinned_models=?,models_filter=? WHERE id=?`,
				p.Name, p.Type, p.BaseURL, p.DefaultModel, p.Priority, b2i(p.Enabled), p.DocURL, p.DocMarkdown, b2i(p.InjectDocs), p.Models, p.Fallback, p.PinnedModels, p.ModelsFilter, p.ID)
		} else {
			_, err = s.db.Exec(`UPDATE providers SET name=?,type=?,base_url=?,api_key_enc=?,default_model=?,priority=?,enabled=?,doc_url=?,doc_markdown=?,inject_docs=?,models=?,fallback=?,pinned_models=?,models_filter=? WHERE id=?`,
				p.Name, p.Type, p.BaseURL, enc, p.DefaultModel, p.Priority, b2i(p.Enabled), p.DocURL, p.DocMarkdown, b2i(p.InjectDocs), p.Models, p.Fallback, p.PinnedModels, p.ModelsFilter, p.ID)
		}
		if err != nil {
			return err
		}
	}
	return s.reload()
}

func (s *Store) DeleteProvider(id int64) error {
	if _, err := s.db.Exec(`DELETE FROM providers WHERE id=?`, id); err != nil {
		return err
	}
	return s.reload()
}

// Reorder sets priority to list position for the given provider IDs.
func (s *Store) Reorder(ids []int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	for i, id := range ids {
		if _, err := tx.Exec(`UPDATE providers SET priority=? WHERE id=?`, (i+1)*10, id); err != nil {
			tx.Rollback()
			return err
		}
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return s.reload()
}

// ---- transforms ----

func (s *Store) Transforms() ([]Transform, error) {
	rows, err := s.db.Query(`SELECT id,name,provider_id,target,phase,rules,enabled FROM transforms ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Transform
	for rows.Next() {
		var t Transform
		var enabled int
		var rules string
		if err := rows.Scan(&t.ID, &t.Name, &t.ProviderID, &t.Target, &t.Phase, &rules, &enabled); err != nil {
			return nil, err
		}
		t.Enabled = enabled == 1
		t.Rules = json.RawMessage(rules)
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *Store) SaveTransform(t *Transform) error {
	if t.Phase != "request" && t.Phase != "response" {
		return errors.New("phase must be request or response")
	}
	var rules []map[string]any
	if err := json.Unmarshal(t.Rules, &rules); err != nil {
		return fmt.Errorf("rules must be a JSON array of ops: %w", err)
	}
	if t.ID == 0 {
		res, err := s.db.Exec(`INSERT INTO transforms(name,provider_id,target,phase,rules,enabled) VALUES(?,?,?,?,?,?)`,
			t.Name, t.ProviderID, t.Target, t.Phase, string(t.Rules), b2i(t.Enabled))
		if err != nil {
			return err
		}
		t.ID, _ = res.LastInsertId()
		return nil
	}
	_, err := s.db.Exec(`UPDATE transforms SET name=?,provider_id=?,target=?,phase=?,rules=?,enabled=? WHERE id=?`,
		t.Name, t.ProviderID, t.Target, t.Phase, string(t.Rules), b2i(t.Enabled), t.ID)
	return err
}

func (s *Store) DeleteTransform(id int64) error {
	_, err := s.db.Exec(`DELETE FROM transforms WHERE id=?`, id)
	return err
}

func (s *Store) SetTransformEnabled(id int64, enabled bool) error {
	_, err := s.db.Exec(`UPDATE transforms SET enabled=? WHERE id=?`, b2i(enabled), id)
	return err
}

// ---- traces ----

func (s *Store) AddTrace(t *Trace) {
	res, err := s.db.Exec(`INSERT INTO traces(ts,provider,model,inbound,stream,status,latency_ms,err,note,prompt_tokens,completion_tokens,cached_tokens,req_snippet,resp_snippet) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		t.TS, t.Provider, t.Model, t.Inbound, b2i(t.Stream), t.Status, t.LatencyMS, t.Err, t.Note, t.PromptTokens, t.CompletionTokens, t.CachedTokens, t.ReqSnip, t.RespSnip)
	if err == nil {
		t.ID, _ = res.LastInsertId()
	}
	// retention: keep newest 5000
	s.db.Exec(`DELETE FROM traces WHERE id <= (SELECT MAX(id) FROM traces) - 5000`)
}

func (s *Store) Traces(afterID int64, limit int) ([]Trace, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.Query(`SELECT id,ts,provider,model,inbound,stream,status,latency_ms,err,note,prompt_tokens,completion_tokens,cached_tokens,req_snippet,resp_snippet FROM traces WHERE id > ? ORDER BY id DESC LIMIT ?`, afterID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Trace
	for rows.Next() {
		var t Trace
		var stream int
		if err := rows.Scan(&t.ID, &t.TS, &t.Provider, &t.Model, &t.Inbound, &stream, &t.Status, &t.LatencyMS, &t.Err, &t.Note, &t.PromptTokens, &t.CompletionTokens, &t.CachedTokens, &t.ReqSnip, &t.RespSnip); err != nil {
			return nil, err
		}
		t.Stream = stream == 1
		out = append(out, t)
	}
	return out, rows.Err()
}

// ---- model map ----

// ModelMap returns the harness-name → provider/model rewrite table
// (settings key "model_map", JSON object).
func (s *Store) ModelMap() map[string]string {
	m := map[string]string{}
	if raw := s.Setting("model_map"); raw != "" {
		json.Unmarshal([]byte(raw), &m)
	}
	return m
}

func (s *Store) SetModelMap(m map[string]string) error {
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return s.SetSetting("model_map", string(b))
}

// ModelMapLookup rewrites a model through the map. Exact-pattern entries win
// over glob patterns; ties broken by pattern order (sorted) for determinism.
func (s *Store) ModelMapLookup(model string, match func(pattern, model string) bool) string {
	m := s.ModelMap()
	if len(m) == 0 {
		return ""
	}
	patterns := make([]string, 0, len(m))
	for k := range m {
		patterns = append(patterns, k)
	}
	sort.Slice(patterns, func(i, j int) bool {
		gi, gj := strings.HasSuffix(patterns[i], "*"), strings.HasSuffix(patterns[j], "*")
		if gi != gj {
			return !gi // exact patterns first
		}
		return patterns[i] < patterns[j]
	})
	for _, pat := range patterns {
		if match(pat, model) {
			return m[pat]
		}
	}
	return ""
}

// ---- per-model stats ----

type ModelStat struct {
	Provider     string `json:"provider"`
	Model        string `json:"model"`
	Requests     int    `json:"requests"`
	Errors       int    `json:"errors"`
	PromptTokens int64  `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	CachedTokens int64  `json:"cached_tokens"`
	AvgLatencyMS int64  `json:"avg_latency_ms"`
}

// Stats aggregates the trace table per provider/model (most-used first).
func (s *Store) Stats() ([]ModelStat, error) {
	rows, err := s.db.Query(`SELECT provider, model,
	  COUNT(*), SUM(CASE WHEN status>=400 OR (err!='' AND err NOT LIKE 'failover%') THEN 1 ELSE 0 END),
	  COALESCE(SUM(prompt_tokens),0), COALESCE(SUM(completion_tokens),0), COALESCE(SUM(cached_tokens),0),
	  COALESCE(AVG(latency_ms),0)
	  FROM traces GROUP BY provider, model ORDER BY COUNT(*) DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ModelStat
	for rows.Next() {
		var m ModelStat
		var avg float64
		if err := rows.Scan(&m.Provider, &m.Model, &m.Requests, &m.Errors,
			&m.PromptTokens, &m.CompletionTokens, &m.CachedTokens, &avg); err != nil {
			return nil, err
		}
		m.AvgLatencyMS = int64(avg)
		out = append(out, m)
	}
	return out, rows.Err()
}

// ---- settings ----

func (s *Store) Setting(k string) string {
	var v string
	s.db.QueryRow(`SELECT v FROM settings WHERE k=?`, k).Scan(&v)
	return v
}

func (s *Store) SetSetting(k, v string) error {
	_, err := s.db.Exec(`INSERT INTO settings(k,v) VALUES(?,?) ON CONFLICT(k) DO UPDATE SET v=excluded.v`, k, v)
	return err
}

// ---- round table logs ----

type RoundtableLog struct {
	ID               int64  `json:"id"`
	TS               int64  `json:"ts"`
	Question         string `json:"question"`
	Profiles         string `json:"profiles"`
	Rounds           int    `json:"rounds"`
	Compressed       bool   `json:"compressed"`
	Moderator        string `json:"moderator"`
	LatencyMS        int64  `json:"latency_ms"`
	PromptTokens     int    `json:"prompt_tokens"`
	CompletionTokens int    `json:"completion_tokens"`
	Output           string `json:"output,omitempty"`
}

func (s *Store) AddRoundtableLog(l *RoundtableLog) error {
	res, err := s.db.Exec(`INSERT INTO roundtable_logs(ts,question,profiles,rounds,compressed,moderator,latency_ms,prompt_tokens,completion_tokens,output) VALUES(?,?,?,?,?,?,?,?,?,?)`,
		l.TS, l.Question, l.Profiles, l.Rounds, b2i(l.Compressed), l.Moderator, l.LatencyMS, l.PromptTokens, l.CompletionTokens, l.Output)
	if err == nil {
		l.ID, _ = res.LastInsertId()
		s.db.Exec(`DELETE FROM roundtable_logs WHERE id <= (SELECT MAX(id) FROM roundtable_logs) - 500`)
	}
	return err
}

// RoundtableLogs returns recent runs without the full output (list view).
func (s *Store) RoundtableLogs(limit int) ([]RoundtableLog, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.db.Query(`SELECT id,ts,question,profiles,rounds,compressed,moderator,latency_ms,prompt_tokens,completion_tokens FROM roundtable_logs ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RoundtableLog
	for rows.Next() {
		var l RoundtableLog
		var comp int
		if err := rows.Scan(&l.ID, &l.TS, &l.Question, &l.Profiles, &l.Rounds, &comp, &l.Moderator, &l.LatencyMS, &l.PromptTokens, &l.CompletionTokens); err != nil {
			return nil, err
		}
		l.Compressed = comp == 1
		out = append(out, l)
	}
	return out, rows.Err()
}

// RoundtableLogByID returns one run with its full output.
func (s *Store) RoundtableLogByID(id int64) (RoundtableLog, bool) {
	var l RoundtableLog
	var comp int
	err := s.db.QueryRow(`SELECT id,ts,question,profiles,rounds,compressed,moderator,latency_ms,prompt_tokens,completion_tokens,output FROM roundtable_logs WHERE id=?`, id).
		Scan(&l.ID, &l.TS, &l.Question, &l.Profiles, &l.Rounds, &comp, &l.Moderator, &l.LatencyMS, &l.PromptTokens, &l.CompletionTokens, &l.Output)
	if err != nil {
		return l, false
	}
	l.Compressed = comp == 1
	return l, true
}

// ---- agent profiles (round-table consensus personas) ----

type AgentProfile struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	Model       string `json:"model"` // provider/model routed through the proxy
	Persona     string `json:"persona"`
	Temperature string `json:"temperature"` // "" = provider default
	Enabled     bool   `json:"enabled"`
}

func (s *Store) AgentProfiles() ([]AgentProfile, error) {
	rows, err := s.db.Query(`SELECT id,name,model,persona,temperature,enabled FROM agent_profiles ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AgentProfile
	for rows.Next() {
		var a AgentProfile
		var en int
		if err := rows.Scan(&a.ID, &a.Name, &a.Model, &a.Persona, &a.Temperature, &en); err != nil {
			return nil, err
		}
		a.Enabled = en == 1
		out = append(out, a)
	}
	return out, rows.Err()
}

func (s *Store) SaveAgentProfile(a *AgentProfile) error {
	if a.Name == "" || a.Model == "" {
		return errors.New("name and model are required")
	}
	if a.ID == 0 {
		res, err := s.db.Exec(`INSERT INTO agent_profiles(name,model,persona,temperature,enabled) VALUES(?,?,?,?,?)`,
			a.Name, a.Model, a.Persona, a.Temperature, b2i(a.Enabled))
		if err != nil {
			return err
		}
		a.ID, _ = res.LastInsertId()
		return nil
	}
	_, err := s.db.Exec(`UPDATE agent_profiles SET name=?,model=?,persona=?,temperature=?,enabled=? WHERE id=?`,
		a.Name, a.Model, a.Persona, a.Temperature, b2i(a.Enabled), a.ID)
	return err
}

func (s *Store) DeleteAgentProfile(id int64) error {
	_, err := s.db.Exec(`DELETE FROM agent_profiles WHERE id=?`, id)
	return err
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}
