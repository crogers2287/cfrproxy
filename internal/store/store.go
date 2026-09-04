// Package store is the persistence layer: SQLite (WAL) with an in-memory
// provider cache on the hot path, and AES-256-GCM encryption for API keys.
package store

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
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
	// ContextLength overrides the context window cfrproxy advertises for this
	// provider's models. 0 = fall back to what the upstream declares, then the
	// "default_context_length" setting. Harnesses read this off /v1/models to
	// size their compaction; guessing from a renamed model id gets it wrong.
	ContextLength int `json:"context_length"`
	// ReasoningEffort is the thinking level sent to this provider's models when
	// the client did not choose one: off|low|medium|high|xhigh, "" = leave the
	// request alone. Local chat templates (Qwen3.8) default to xhigh when the
	// request carries nothing, which is rarely what an agent harness wants.
	// ReasoningForce makes it override a level the client DID send.
	ReasoningEffort string `json:"reasoning_effort"`
	ReasoningForce  bool   `json:"reasoning_force"`
	// Caveman enables payload compression of bulky tool results sent to this
	// provider. Off by default; see internal/proxy/caveman.go.
	Caveman bool `json:"caveman"`
	// NoFallback stops a failed request to this provider being re-routed to any
	// other. The caller gets the real error instead of a silent switch.
	NoFallback bool `json:"no_fallback"`
	// Headers is a JSON object of extra outbound headers to set on every
	// request to this provider (name -> value). A value of "@file:<path>"
	// is read from the file on every request, so a CLI auth token that the
	// upstream rotates keeps working without a proxy restart. Injected
	// headers override the defaults (Authorization/x-api-key) — this is how
	// a proxy routes harness traffic while sending the exact Authorization
	// and User-Agent a native CLI sends.
	Headers string `json:"headers"`
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
	ID               int64  `json:"id"`
	TS               int64  `json:"ts"`
	Provider         string `json:"provider"`
	Model            string `json:"model"`
	Inbound          string `json:"inbound"`
	Stream           bool   `json:"stream"`
	Status           int    `json:"status"`
	LatencyMS        int64  `json:"latency_ms"`
	Err              string `json:"err"`
	Note             string `json:"note"`
	PromptTokens     int    `json:"prompt_tokens"`
	CompletionTokens int    `json:"completion_tokens"`
	CachedTokens     int    `json:"cached_tokens"`
	// TTFBMS is time-to-first-byte from the upstream: prompt processing plus
	// queue/load time, before any token is produced. PostMS is what cfrproxy
	// itself spent after the upstream finished (dialect translation, transform
	// rules, relay teardown) — proxy overhead, isolated from model time so a
	// slow call can be blamed correctly.
	TTFBMS int64 `json:"ttfb_ms"`
	// microseconds, not milliseconds: cfrproxy's trailing work is routinely
	// sub-millisecond, and truncating it to "0ms" told the operator nothing.
	PostUS int64 `json:"post_us"`
	// Caveman compression accounting for this request (0 when disabled).
	// CMMode is the resolved mode: "" = caveman not involved, else off|in|out|both.
	CMMode   string `json:"cm_mode"`
	CMMsgs   int    `json:"cm_msgs"`
	CMBefore int    `json:"cm_before"`
	CMAfter  int    `json:"cm_after"`
	ReqSnip  string `json:"req_snippet"`
	RespSnip string `json:"resp_snippet"`

	// Prefix-cache telemetry from llama.cpp-family upstreams (the `timings`
	// object). Deliberately NOT persisted to SQLite — AddTrace names its
	// columns explicitly, so these ride along in memory only, which is enough
	// for the SSE hub, the admin API, and the JSONL cache log. Keeping them out
	// of the schema avoids migrating a live 22 MB DB for observability data
	// that is already durably recorded in cache-observability.jsonl.
	Client           string  `json:"client,omitempty"`
	CacheLCP         int     `json:"cache_lcp_n,omitempty"`
	CacheReprocessed int     `json:"cache_reprocessed_n,omitempty"`
	CacheSource      string  `json:"cache_source,omitempty"`
	CacheReason      string  `json:"cache_reason,omitempty"`
	PromptMS         float64 `json:"prompt_ms,omitempty"`
	PromptTPS        float64 `json:"prompt_per_second,omitempty"`
	DecodeTPS        float64 `json:"predicted_per_second,omitempty"`
}

// GenMS is the time the model spent actually producing tokens: total latency
// minus the upstream's think-time before the first byte, minus cfrproxy's own
// post-processing. Falls back to total latency when TTFB was never recorded.
func (t Trace) GenMS() int64 {
	g := t.LatencyMS - t.TTFBMS - t.PostUS/1000
	if g <= 0 {
		g = t.LatencyMS
	}
	return g
}

// PromptPerSec is the prefill rate — llama.cpp's "pp": how fast the prompt was
// ingested, in tokens/sec, measured over the window before the first output
// token appeared.
//
// Only meaningful on a STREAMED response, where time-to-first-token is
// observable. A non-streamed upstream withholds headers until the whole
// completion is done, so prefill and generation are indistinguishable from
// outside and this returns 0 rather than a fabricated split.
//
// Cached prompt tokens are excluded: they were never re-processed, so counting
// them would inflate the rate by exactly the cache hit ratio.
func (t Trace) PromptPerSec() float64 {
	if !t.Stream || t.TTFBMS <= 0 {
		return 0
	}
	n := t.PromptTokens - t.CachedTokens
	if n <= 0 {
		return 0
	}
	return float64(n) / (float64(t.TTFBMS) / 1000)
}

// TokensPerSec is the output generation rate — llama.cpp's "tg". Zero when it cannot be computed
// honestly (no completion tokens, or no measurable generation window).
func (t Trace) TokensPerSec() float64 {
	g := t.GenMS()
	if t.CompletionTokens <= 0 || g <= 0 {
		return 0
	}
	return float64(t.CompletionTokens) / (float64(g) / 1000)
}

type Store struct {
	db  *sql.DB
	key []byte // AES-256 key for API-key encryption

	mu          sync.RWMutex
	cache       []Provider // decrypted, sorted by priority — the hot-path registry
	settings    map[string]string
	transforms  []Transform
	endpoints   []Endpoint
	dataVersion int64     // SQLite data_version at last reload; detects writes from other processes
	lastProbe   time.Time // last PRAGMA data_version probe; maybeReload is throttled to reloadProbeEvery

	stopOnce      sync.Once
	stopRetention chan struct{}

	admin adminCache // verified admin credentials, see admin.go
}

// reloadProbeEvery bounds how often maybeReload asks SQLite for data_version.
// Every settings/provider/transform/endpoint read used to be its own query on
// the single connection — 15-25 serialized round-trips per proxied request.
// Now they are served from memory and a change made by another process (the
// CLI) is picked up within this window; our own writes refresh immediately.
const reloadProbeEvery = 200 * time.Millisecond

// Retention caps. Pruning runs on a timer (see retentionLoop) rather than on
// every insert, which used to add a subquery+DELETE to each request.
var (
	KeepTraces         = 5000
	KeepRoundtableLogs = 500
	retentionEvery     = time.Minute
)

var ValidTypes = map[string]bool{"openai": true, "anthropic": true, "ollama": true, "commandcode": true}

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
	s.stopRetention = make(chan struct{})
	go s.retentionLoop()
	return s, nil
}

func (s *Store) Close() error {
	s.stopOnce.Do(func() { close(s.stopRetention) })
	return s.db.Close()
}

func (s *Store) retentionLoop() {
	t := time.NewTicker(retentionEvery)
	defer t.Stop()
	for {
		select {
		case <-s.stopRetention:
			return
		case <-t.C:
			s.PruneRetention()
		}
	}
}

// PruneRetention drops traces and round-table logs beyond the retention caps.
func (s *Store) PruneRetention() {
	s.db.Exec(`DELETE FROM traces WHERE id <= (SELECT MAX(id) FROM traces) - ?`, KeepTraces)
	s.db.Exec(`DELETE FROM roundtable_logs WHERE id <= (SELECT MAX(id) FROM roundtable_logs) - ?`, KeepRoundtableLogs)
}

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
  models_filter TEXT NOT NULL DEFAULT '',
  context_length INTEGER NOT NULL DEFAULT 0,
  headers TEXT NOT NULL DEFAULT ''
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
  ttfb_ms INTEGER NOT NULL DEFAULT 0,
  post_us INTEGER NOT NULL DEFAULT 0,
  req_snippet TEXT NOT NULL DEFAULT '',
  resp_snippet TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS traces_ts ON traces(ts);
CREATE TABLE IF NOT EXISTS settings (k TEXT PRIMARY KEY, v TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS routers (
  id INTEGER PRIMARY KEY,
  name TEXT UNIQUE NOT NULL,
  enabled INTEGER NOT NULL DEFAULT 1,
  classifier TEXT NOT NULL DEFAULT '',
  planner TEXT NOT NULL DEFAULT '',
  routes TEXT NOT NULL DEFAULT '{}'
);
CREATE TABLE IF NOT EXISTS fusions (
  id INTEGER PRIMARY KEY,
  name TEXT UNIQUE NOT NULL,
  enabled INTEGER NOT NULL DEFAULT 1,
  participants TEXT NOT NULL DEFAULT '[]',
  judge TEXT NOT NULL DEFAULT '',
  max_tokens INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS endpoints (
  id INTEGER PRIMARY KEY,
  name TEXT UNIQUE NOT NULL,
  api_key_enc BLOB,
  models TEXT NOT NULL DEFAULT '',
  force_model TEXT NOT NULL DEFAULT '',
  enabled INTEGER NOT NULL DEFAULT 1,
  note TEXT NOT NULL DEFAULT ''
);
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
	s.db.Exec(`ALTER TABLE providers ADD COLUMN context_length INTEGER NOT NULL DEFAULT 0`)
	s.db.Exec(`ALTER TABLE providers ADD COLUMN headers TEXT NOT NULL DEFAULT ''`)
	s.db.Exec(`ALTER TABLE traces ADD COLUMN note TEXT NOT NULL DEFAULT ''`)
	s.db.Exec(`ALTER TABLE traces ADD COLUMN prompt_tokens INTEGER NOT NULL DEFAULT 0`)
	s.db.Exec(`ALTER TABLE traces ADD COLUMN completion_tokens INTEGER NOT NULL DEFAULT 0`)
	s.db.Exec(`ALTER TABLE traces ADD COLUMN cached_tokens INTEGER NOT NULL DEFAULT 0`)
	// Caveman payload compression: opt-in per provider and per share endpoint
	// (default 0 = off), plus per-trace byte accounting so the WebUI can show
	// what it actually saved on live traffic rather than a claimed ratio.
	s.db.Exec(`ALTER TABLE providers ADD COLUMN caveman INTEGER NOT NULL DEFAULT 0`)
	s.db.Exec(`ALTER TABLE endpoints ADD COLUMN caveman INTEGER NOT NULL DEFAULT 0`)
	s.db.Exec(`ALTER TABLE traces ADD COLUMN cm_msgs INTEGER NOT NULL DEFAULT 0`)
	s.db.Exec(`ALTER TABLE traces ADD COLUMN cm_before INTEGER NOT NULL DEFAULT 0`)
	s.db.Exec(`ALTER TABLE traces ADD COLUMN cm_after INTEGER NOT NULL DEFAULT 0`)
	// cm_mode records the RESOLVED caveman mode for the request. Byte counts
	// alone are ambiguous: cm_msgs=0 could mean the caller never asked for
	// compression, asked and disabled it (--mode off), or asked and nothing
	// qualified. Those need different responses, so store which one it was.
	s.db.Exec(`ALTER TABLE traces ADD COLUMN cm_mode TEXT NOT NULL DEFAULT ''`)
	// no_fallback: pin a request to its chosen provider. Silent failover is
	// usually right, but not always — a local/free provider going down otherwise
	// reroutes to a paid one without the caller noticing, and a share endpoint
	// handed to someone else should not spend on providers the recipient was
	// never granted. Default 0 keeps existing behaviour.
	s.db.Exec(`ALTER TABLE providers ADD COLUMN no_fallback INTEGER NOT NULL DEFAULT 0`)
	s.db.Exec(`ALTER TABLE providers ADD COLUMN reasoning_effort TEXT NOT NULL DEFAULT ''`)
	s.db.Exec(`ALTER TABLE providers ADD COLUMN reasoning_force INTEGER NOT NULL DEFAULT 0`)
	s.db.Exec(`ALTER TABLE endpoints ADD COLUMN no_fallback INTEGER NOT NULL DEFAULT 0`)
	s.db.Exec(`ALTER TABLE endpoints ADD COLUMN context_length INTEGER NOT NULL DEFAULT 0`)
	s.db.Exec(`ALTER TABLE endpoints ADD COLUMN reasoning_effort TEXT NOT NULL DEFAULT ''`)
	s.db.Exec(`ALTER TABLE endpoints ADD COLUMN reasoning_force INTEGER NOT NULL DEFAULT 0`)
	s.db.Exec(`ALTER TABLE usage_daily ADD COLUMN absorbed INTEGER NOT NULL DEFAULT 0`)
	// usage_daily: durable per-day/provider/model accounting. `traces` is a
	// rolling 5000-row buffer (~22 h at current volume), which is useless for
	// answering "what burned my plan this week" — that gap is exactly why a
	// blown ccbudget weekly limit could only be reconstructed from an agent's
	// own SQLite afterwards. This table is append/upsert-only and never pruned.
	// fails_* / status_* let a provider that 429s and silently falls through the
	// fallback chain be spotted from usage alone.
	s.db.Exec(`CREATE TABLE IF NOT EXISTS usage_daily (
	  day TEXT NOT NULL,
	  provider TEXT NOT NULL DEFAULT '',
	  model TEXT NOT NULL DEFAULT '',
	  requests INTEGER NOT NULL DEFAULT 0,
	  ok INTEGER NOT NULL DEFAULT 0,
	  failed INTEGER NOT NULL DEFAULT 0,
	  fellback INTEGER NOT NULL DEFAULT 0,
	  status_429 INTEGER NOT NULL DEFAULT 0,
	  status_4xx INTEGER NOT NULL DEFAULT 0,
	  status_5xx INTEGER NOT NULL DEFAULT 0,
	  prompt_tokens INTEGER NOT NULL DEFAULT 0,
	  completion_tokens INTEGER NOT NULL DEFAULT 0,
	  cached_tokens INTEGER NOT NULL DEFAULT 0,
	  no_usage INTEGER NOT NULL DEFAULT 0,
	  absorbed INTEGER NOT NULL DEFAULT 0,
	  last_err TEXT NOT NULL DEFAULT '',
	  updated_at INTEGER NOT NULL DEFAULT 0,
	  PRIMARY KEY(day,provider,model)
	)`)
	// One-time backfill from whatever is still in the rolling traces buffer, so
	// the rollup is useful the moment it ships instead of a week later.
	var seeded int
	s.db.QueryRow(`SELECT COUNT(*) FROM usage_daily`).Scan(&seeded)
	if seeded == 0 {
		s.db.Exec(`INSERT INTO usage_daily(day,provider,model,requests,ok,failed,fellback,status_429,status_4xx,status_5xx,prompt_tokens,completion_tokens,cached_tokens,no_usage,absorbed,last_err,updated_at)
		 SELECT strftime('%Y-%m-%d', CASE WHEN ts > 1000000000000 THEN ts/1000 ELSE ts END, 'unixepoch'),
		        provider, model, COUNT(*),
		        SUM(CASE WHEN status>0 AND status<400 AND (err='' OR err LIKE '%failover from%') THEN 1 ELSE 0 END),
		        SUM(CASE WHEN status>=400 OR (err!='' AND err NOT LIKE '%failover from%') THEN 1 ELSE 0 END),
		        SUM(CASE WHEN note LIKE '%fallback%' THEN 1 ELSE 0 END),
		        SUM(CASE WHEN status=429 THEN 1 ELSE 0 END),
		        SUM(CASE WHEN status>=400 AND status<500 AND status!=429 THEN 1 ELSE 0 END),
		        SUM(CASE WHEN status>=500 THEN 1 ELSE 0 END),
		        SUM(COALESCE(prompt_tokens,0)), SUM(COALESCE(completion_tokens,0)), SUM(COALESCE(cached_tokens,0)),
		        SUM(CASE WHEN COALESCE(prompt_tokens,0)=0 AND COALESCE(completion_tokens,0)=0 THEN 1 ELSE 0 END),
		        SUM(CASE WHEN status>0 AND status<400 AND err LIKE '%failover from%' THEN 1 ELSE 0 END),
		        '', strftime('%s','now')
		 FROM traces GROUP BY 1,2,3`)
	}
	s.db.Exec(`ALTER TABLE traces ADD COLUMN ttfb_ms INTEGER NOT NULL DEFAULT 0`)
	s.db.Exec(`ALTER TABLE traces ADD COLUMN post_us INTEGER NOT NULL DEFAULT 0`)
	// Skill index: roots to scan, the indexed SKILL.md cache, and per-endpoint
	// assignments. See store/skills.go. Created here (idempotent) so an existing
	// db picks them up on next start.
	s.db.Exec(`CREATE TABLE IF NOT EXISTS skill_roots (
  id INTEGER PRIMARY KEY,
  path TEXT UNIQUE NOT NULL,
  label TEXT NOT NULL DEFAULT '',
  enabled INTEGER NOT NULL DEFAULT 1
)`)
	s.db.Exec(`CREATE TABLE IF NOT EXISTS skills (
  id INTEGER PRIMARY KEY,
  name TEXT NOT NULL DEFAULT '',
  description TEXT NOT NULL DEFAULT '',
  path TEXT UNIQUE NOT NULL,
  root_id INTEGER NOT NULL DEFAULT 0,
  rel_dir TEXT NOT NULL DEFAULT '',
  is_symlink INTEGER NOT NULL DEFAULT 0,
  symlink_target TEXT NOT NULL DEFAULT '',
  size INTEGER NOT NULL DEFAULT 0,
  mtime INTEGER NOT NULL DEFAULT 0,
  sha TEXT NOT NULL DEFAULT '',
  tags TEXT NOT NULL DEFAULT '',
  scanned_at INTEGER NOT NULL DEFAULT 0
)`)
	s.db.Exec(`CREATE INDEX IF NOT EXISTS skills_name ON skills(name)`)
	s.db.Exec(`CREATE TABLE IF NOT EXISTS skill_assignments (
  id INTEGER PRIMARY KEY,
  target_kind TEXT NOT NULL DEFAULT 'endpoint',
  target_id INTEGER NOT NULL,
  model_glob TEXT NOT NULL DEFAULT '*',
  skill_id INTEGER NOT NULL,
  enabled INTEGER NOT NULL DEFAULT 1,
  UNIQUE(target_kind, target_id, model_glob, skill_id)
)`)
	s.migrateSkillGroups()
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
	rows, err := s.db.Query(`SELECT id,name,type,base_url,api_key_enc,default_model,priority,enabled,doc_url,doc_markdown,inject_docs,models,fallback,pinned_models,caveman,no_fallback,models_filter,context_length,headers,reasoning_effort,reasoning_force FROM providers`)
	if err != nil {
		return err
	}
	defer rows.Close()
	var out []Provider
	for rows.Next() {
		var p Provider
		var enc []byte
		var enabled, inject, cave, nofb int
		var rforce int
		if err := rows.Scan(&p.ID, &p.Name, &p.Type, &p.BaseURL, &enc, &p.DefaultModel, &p.Priority, &enabled, &p.DocURL, &p.DocMarkdown, &inject, &p.Models, &p.Fallback, &p.PinnedModels, &cave, &nofb, &p.ModelsFilter, &p.ContextLength, &p.Headers, &p.ReasoningEffort, &rforce); err != nil {
			return err
		}
		p.Enabled, p.InjectDocs, p.Caveman, p.NoFallback = enabled == 1, inject == 1, cave == 1, nofb == 1
		p.ReasoningForce = rforce == 1
		if p.APIKey, err = s.decrypt(enc); err != nil {
			return fmt.Errorf("provider %s: %w", p.Name, err)
		}
		p.HasKey = p.APIKey != ""
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Priority < out[j].Priority })
	if err := rows.Err(); err != nil {
		return err
	}
	rows.Close()
	settings, err := s.loadSettings()
	if err != nil {
		return err
	}
	transforms, err := s.loadTransforms()
	if err != nil {
		return err
	}
	endpoints, err := s.loadEndpoints()
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.cache, s.settings, s.transforms, s.endpoints = out, settings, transforms, endpoints
	s.mu.Unlock()
	return nil
}

func (s *Store) loadSettings() (map[string]string, error) {
	rows, err := s.db.Query(`SELECT k,v FROM settings`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	m := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		m[k] = v
	}
	return m, rows.Err()
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
	s.mu.RLock()
	recent := time.Since(s.lastProbe) < reloadProbeEvery
	s.mu.RUnlock()
	if recent {
		return
	}
	var v int64
	if err := s.db.QueryRow(`PRAGMA data_version`).Scan(&v); err != nil {
		return
	}
	s.mu.Lock()
	stale := v != s.dataVersion
	s.lastProbe = time.Now()
	s.mu.Unlock()
	if stale {
		s.reload()
		s.mu.Lock()
		s.dataVersion = v
		s.mu.Unlock()
	}
}

// ProviderByName resolves a provider by name. Exact match wins; failing that
// it retries trimmed + case-insensitively, so a scoped mount like
// /p/qwen/v1/models (or a stale "/p/Qwen%20/" carrying a trailing space from
// an old config) still finds the provider named "Qwen". This matches how
// ResolveModel already compares the "provider/model" prefix (EqualFold) — a
// case-sensitive lookup here silently returned an empty model list instead.
func (s *Store) ProviderByName(name string) (Provider, bool) {
	provs := s.Providers()
	for _, p := range provs {
		if p.Name == name {
			return p, true
		}
	}
	want := strings.TrimSpace(name)
	if want == "" {
		return Provider{}, false
	}
	for _, p := range provs {
		if strings.EqualFold(strings.TrimSpace(p.Name), want) {
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
	lvl, err := NormalizeReasoning(p.ReasoningEffort)
	if err != nil {
		return err
	}
	p.ReasoningEffort = lvl
	if !ValidTypes[p.Type] {
		return fmt.Errorf("invalid provider type %q (want openai|anthropic|ollama)", p.Type)
	}
	// Trim first: a stray trailing space in a provider name is invisible in the
	// UI but breaks every scoped mount built from it (/p/<name>/v1).
	p.Name = strings.TrimSpace(p.Name)
	p.BaseURL = strings.TrimSpace(p.BaseURL)
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
		res, err := s.db.Exec(`INSERT INTO providers(name,type,base_url,api_key_enc,default_model,priority,enabled,doc_url,doc_markdown,inject_docs,models,fallback,pinned_models,caveman,no_fallback,models_filter,context_length,headers,reasoning_effort,reasoning_force) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			p.Name, p.Type, p.BaseURL, enc, p.DefaultModel, p.Priority, b2i(p.Enabled), p.DocURL, p.DocMarkdown, b2i(p.InjectDocs), p.Models, p.Fallback, p.PinnedModels, b2i(p.Caveman), b2i(p.NoFallback), p.ModelsFilter, p.ContextLength, p.Headers, p.ReasoningEffort, b2i(p.ReasoningForce))
		if err != nil {
			return err
		}
		p.ID, _ = res.LastInsertId()
	} else {
		// empty APIKey on update = keep existing key
		if p.APIKey == "" {
			_, err = s.db.Exec(`UPDATE providers SET name=?,type=?,base_url=?,default_model=?,priority=?,enabled=?,doc_url=?,doc_markdown=?,inject_docs=?,models=?,fallback=?,pinned_models=?,caveman=?,no_fallback=?,models_filter=?,context_length=?,headers=?,reasoning_effort=?,reasoning_force=? WHERE id=?`,
				p.Name, p.Type, p.BaseURL, p.DefaultModel, p.Priority, b2i(p.Enabled), p.DocURL, p.DocMarkdown, b2i(p.InjectDocs), p.Models, p.Fallback, p.PinnedModels, b2i(p.Caveman), b2i(p.NoFallback), p.ModelsFilter, p.ContextLength, p.Headers, p.ReasoningEffort, b2i(p.ReasoningForce), p.ID)
		} else {
			_, err = s.db.Exec(`UPDATE providers SET name=?,type=?,base_url=?,api_key_enc=?,default_model=?,priority=?,enabled=?,doc_url=?,doc_markdown=?,inject_docs=?,models=?,fallback=?,pinned_models=?,caveman=?,no_fallback=?,models_filter=?,context_length=?,headers=?,reasoning_effort=?,reasoning_force=? WHERE id=?`,
				p.Name, p.Type, p.BaseURL, enc, p.DefaultModel, p.Priority, b2i(p.Enabled), p.DocURL, p.DocMarkdown, b2i(p.InjectDocs), p.Models, p.Fallback, p.PinnedModels, b2i(p.Caveman), b2i(p.NoFallback), p.ModelsFilter, p.ContextLength, p.Headers, p.ReasoningEffort, b2i(p.ReasoningForce), p.ID)
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

// Transforms returns the cached transform list (see maybeReload).
func (s *Store) Transforms() ([]Transform, error) {
	s.maybeReload()
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Transform, len(s.transforms))
	copy(out, s.transforms)
	return out, nil
}

func (s *Store) loadTransforms() ([]Transform, error) {
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

// SaveTransform persists t and refreshes the in-memory caches.
func (s *Store) SaveTransform(t *Transform) error {
	if err := s.saveTransform(t); err != nil {
		return err
	}
	return s.reload()
}

func (s *Store) saveTransform(t *Transform) error {
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
	if err != nil {
		return err
	}
	return s.reload()
}

func (s *Store) SetTransformEnabled(id int64, enabled bool) error {
	_, err := s.db.Exec(`UPDATE transforms SET enabled=? WHERE id=?`, b2i(enabled), id)
	return err
}

// ---- traces ----

func (s *Store) AddTrace(t *Trace) {
	res, err := s.db.Exec(`INSERT INTO traces(ts,provider,model,inbound,stream,status,latency_ms,err,note,prompt_tokens,completion_tokens,cached_tokens,ttfb_ms,post_us,req_snippet,resp_snippet,cm_msgs,cm_before,cm_after,cm_mode) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		t.TS, t.Provider, t.Model, t.Inbound, b2i(t.Stream), t.Status, t.LatencyMS, t.Err, t.Note, t.PromptTokens, t.CompletionTokens, t.CachedTokens, t.TTFBMS, t.PostUS, t.ReqSnip, t.RespSnip, t.CMMsgs, t.CMBefore, t.CMAfter, t.CMMode)
	if err == nil {
		t.ID, _ = res.LastInsertId()
	}
	s.rollupUsage(t)
}

// rollupUsage folds a trace into the durable usage_daily table. Best-effort:
// never blocks or fails a request. `no_usage` counts responses that carried no
// token accounting at all — the signal that a provider/route is invisible to
// billing oversight (grok and ccbudget were both at 100% no_usage when this
// was written).
func (s *Store) rollupUsage(t *Trace) {
	if t == nil {
		return
	}
	ts := t.TS
	if ts > 1e12 { // stored in ms
		ts /= 1000
	}
	day := time.Unix(ts, 0).UTC().Format("2006-01-02")
	ok, failed, fellback := 0, 0, 0
	absorbed := 0
	s429, s4, s5 := 0, 0, 0
	switch {
	case t.Status == 429:
		s429, failed = 1, 1
	case t.Status >= 500:
		s5, failed = 1, 1
	case t.Status >= 400:
		s4, failed = 1, 1
	case t.Status > 0:
		ok = 1
	}
	// A 2xx trace can still carry an Err string describing why an EARLIER
	// provider failed ("failover from grok (…)") — that is a successful
	// absorption of someone else's failure, not a failure here. Counting it as
	// failed made codex look 96% broken while it was in fact soaking up all the
	// traffic from three exhausted providers.
	if t.Err != "" && ok == 1 && !strings.Contains(t.Err, "failover from") {
		ok, failed = 0, 1
	}
	if ok == 1 && strings.Contains(t.Err, "failover from") {
		absorbed = 1
	}
	if strings.Contains(t.Note, "fallback") {
		fellback = 1
	}
	noUsage := 0
	if t.PromptTokens == 0 && t.CompletionTokens == 0 {
		noUsage = 1
	}
	errSnip := t.Err
	if len(errSnip) > 300 {
		errSnip = errSnip[:300]
	}
	s.db.Exec(`INSERT INTO usage_daily(day,provider,model,requests,ok,failed,fellback,status_429,status_4xx,status_5xx,prompt_tokens,completion_tokens,cached_tokens,no_usage,absorbed,last_err,updated_at)
	 VALUES(?,?,?,1,?,?,?,?,?,?,?,?,?,?,?,?,?)
	 ON CONFLICT(day,provider,model) DO UPDATE SET
	   requests=requests+1, ok=ok+excluded.ok, failed=failed+excluded.failed,
	   fellback=fellback+excluded.fellback, status_429=status_429+excluded.status_429,
	   status_4xx=status_4xx+excluded.status_4xx, status_5xx=status_5xx+excluded.status_5xx,
	   prompt_tokens=prompt_tokens+excluded.prompt_tokens,
	   completion_tokens=completion_tokens+excluded.completion_tokens,
	   cached_tokens=cached_tokens+excluded.cached_tokens,
	   no_usage=no_usage+excluded.no_usage, absorbed=absorbed+excluded.absorbed,
	   last_err=CASE WHEN excluded.last_err!='' THEN excluded.last_err ELSE last_err END,
	   updated_at=excluded.updated_at`,
		day, t.Provider, t.Model, ok, failed, fellback, s429, s4, s5,
		t.PromptTokens, t.CompletionTokens, t.CachedTokens, noUsage, absorbed, errSnip, time.Now().Unix())
}

// UsageDaily returns durable usage rows, newest day first.
func (s *Store) UsageDaily(sinceDay string, limit int) ([]map[string]any, error) {
	if limit <= 0 || limit > 2000 {
		limit = 500
	}
	rows, err := s.db.Query(`SELECT day,provider,model,requests,ok,failed,fellback,status_429,status_4xx,status_5xx,
	  prompt_tokens,completion_tokens,cached_tokens,no_usage,absorbed,last_err
	  FROM usage_daily WHERE day >= ? ORDER BY day DESC, prompt_tokens DESC LIMIT ?`, sinceDay, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var day, prov, model, lastErr string
		var req, ok, failed, fb, s429, s4, s5, pt, ct, cached, nu, ab int64
		if err := rows.Scan(&day, &prov, &model, &req, &ok, &failed, &fb, &s429, &s4, &s5, &pt, &ct, &cached, &nu, &ab, &lastErr); err != nil {
			continue
		}
		hit := 0.0
		if pt > 0 {
			hit = float64(cached) / float64(pt)
		}
		out = append(out, map[string]any{
			"day": day, "provider": prov, "model": model,
			"requests": req, "ok": ok, "failed": failed, "fellback": fb,
			"status_429": s429, "status_4xx": s4, "status_5xx": s5,
			"prompt_tokens": pt, "completion_tokens": ct, "cached_tokens": cached,
			"cache_hit_rate": hit, "no_usage": nu, "absorbed": ab, "last_err": lastErr,
		})
	}
	return out, rows.Err()
}

func (s *Store) Traces(afterID int64, limit int) ([]Trace, error) {
	if limit <= 0 || limit > 500 {
		limit = 100
	}
	rows, err := s.db.Query(`SELECT id,ts,provider,model,inbound,stream,status,latency_ms,err,note,prompt_tokens,completion_tokens,cached_tokens,ttfb_ms,post_us,cm_msgs,cm_before,cm_after,cm_mode,req_snippet,resp_snippet FROM traces WHERE id > ? ORDER BY id DESC LIMIT ?`, afterID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Trace
	for rows.Next() {
		var t Trace
		var stream int
		if err := rows.Scan(&t.ID, &t.TS, &t.Provider, &t.Model, &t.Inbound, &stream, &t.Status, &t.LatencyMS, &t.Err, &t.Note, &t.PromptTokens, &t.CompletionTokens, &t.CachedTokens, &t.TTFBMS, &t.PostUS, &t.CMMsgs, &t.CMBefore, &t.CMAfter, &t.CMMode, &t.ReqSnip, &t.RespSnip); err != nil {
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
	Provider         string `json:"provider"`
	Model            string `json:"model"`
	Requests         int    `json:"requests"`
	Errors           int    `json:"errors"`
	PromptTokens     int64  `json:"prompt_tokens"`
	CompletionTokens int64  `json:"completion_tokens"`
	CachedTokens     int64  `json:"cached_tokens"`
	AvgLatencyMS     int64  `json:"avg_latency_ms"`
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

// Setting returns a settings value from the in-memory cache (see maybeReload).
func (s *Store) Setting(k string) string {
	s.maybeReload()
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.settings[k]
}

func (s *Store) SetSetting(k, v string) error {
	_, err := s.db.Exec(`INSERT INTO settings(k,v) VALUES(?,?) ON CONFLICT(k) DO UPDATE SET v=excluded.v`, k, v)
	if err != nil {
		return err
	}
	s.mu.Lock()
	if s.settings == nil {
		s.settings = map[string]string{}
	}
	s.settings[k] = v
	s.mu.Unlock()
	return nil
}

// ---- named auto-routers ----

type Router struct {
	ID         int64           `json:"id"`
	Name       string          `json:"name"`
	Enabled    bool            `json:"enabled"`
	Classifier string          `json:"classifier"`
	Planner    string          `json:"planner"`
	Routes     json.RawMessage `json:"routes"` // {bucket: provider/model}
}

func (s *Store) Routers() ([]Router, error) {
	rows, err := s.db.Query(`SELECT id,name,enabled,classifier,planner,routes FROM routers ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Router
	for rows.Next() {
		var r Router
		var en int
		var routes string
		if err := rows.Scan(&r.ID, &r.Name, &en, &r.Classifier, &r.Planner, &routes); err != nil {
			return nil, err
		}
		r.Enabled = en == 1
		r.Routes = json.RawMessage(routes)
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *Store) RouterByName(name string) (Router, bool) {
	rs, _ := s.Routers()
	for _, r := range rs {
		if strings.EqualFold(r.Name, name) {
			return r, true
		}
	}
	return Router{}, false
}

func (s *Store) SaveRouter(r *Router) error {
	r.Name = strings.TrimSpace(r.Name)
	if r.Name == "" {
		return errors.New("router name is required")
	}
	if strings.ContainsAny(r.Name, "/ :?#") {
		return errors.New("router name must be a plain slug (no spaces, / : ? #)")
	}
	routes := string(r.Routes)
	if routes == "" {
		routes = "{}"
	}
	if !json.Valid([]byte(routes)) {
		return errors.New("routes must be a JSON object")
	}
	if r.ID == 0 {
		res, err := s.db.Exec(`INSERT INTO routers(name,enabled,classifier,planner,routes) VALUES(?,?,?,?,?)`,
			r.Name, b2i(r.Enabled), r.Classifier, r.Planner, routes)
		if err != nil {
			return err
		}
		r.ID, _ = res.LastInsertId()
		return nil
	}
	_, err := s.db.Exec(`UPDATE routers SET name=?,enabled=?,classifier=?,planner=?,routes=? WHERE id=?`,
		r.Name, b2i(r.Enabled), r.Classifier, r.Planner, routes, r.ID)
	return err
}

// Fusion is a named fusion pipeline, the direct analogue of Router: several
// participant models answer in parallel and a judge synthesizes one reply.
// The settings-key "fusion" remains the unnamed default; these are the custom
// ones, addressable as "fusion:NAME" the way routers are "auto:NAME".
type Fusion struct {
	ID           int64    `json:"id"`
	Name         string   `json:"name"`
	Enabled      bool     `json:"enabled"`
	Participants []string `json:"participants"` // "provider/model" refs
	Judge        string   `json:"judge"`        // "provider/model" that synthesizes
	MaxTokens    int      `json:"max_tokens"`   // 0 = inherit the default
}

func (s *Store) Fusions() ([]Fusion, error) {
	rows, err := s.db.Query(`SELECT id,name,enabled,participants,judge,max_tokens FROM fusions ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Fusion
	for rows.Next() {
		var f Fusion
		var en int
		var parts string
		if err := rows.Scan(&f.ID, &f.Name, &en, &parts, &f.Judge, &f.MaxTokens); err != nil {
			return nil, err
		}
		f.Enabled = en == 1
		// Tolerate a malformed row rather than failing the whole listing: a
		// broken fusion should disable itself, not hide every other one.
		_ = json.Unmarshal([]byte(parts), &f.Participants)
		out = append(out, f)
	}
	return out, rows.Err()
}

func (s *Store) FusionByName(name string) (Fusion, bool) {
	fs, _ := s.Fusions()
	for _, f := range fs {
		if strings.EqualFold(f.Name, name) {
			return f, true
		}
	}
	return Fusion{}, false
}

func (s *Store) SaveFusion(f *Fusion) error {
	f.Name = strings.TrimSpace(f.Name)
	if f.Name == "" {
		return errors.New("fusion name is required")
	}
	// Same slug rule as routers: the name becomes part of a model id
	// ("fusion:NAME"), so a separator in it would split wrong downstream.
	if strings.ContainsAny(f.Name, "/ :?#") {
		return errors.New("fusion name must be a plain slug (no spaces, / : ? #)")
	}
	var parts []string
	for _, p := range f.Participants {
		if p = strings.TrimSpace(p); p != "" {
			parts = append(parts, p)
		}
	}
	f.Participants = parts
	if f.Judge = strings.TrimSpace(f.Judge); f.Judge == "" {
		return errors.New("fusion judge is required")
	}
	if len(parts) == 0 {
		return errors.New("fusion needs at least one participant")
	}
	if f.MaxTokens < 0 {
		f.MaxTokens = 0
	}
	blob, err := json.Marshal(parts)
	if err != nil {
		return err
	}
	if f.ID == 0 {
		res, err := s.db.Exec(`INSERT INTO fusions(name,enabled,participants,judge,max_tokens) VALUES(?,?,?,?,?)`,
			f.Name, b2i(f.Enabled), string(blob), f.Judge, f.MaxTokens)
		if err != nil {
			return err
		}
		f.ID, _ = res.LastInsertId()
		return nil
	}
	_, err = s.db.Exec(`UPDATE fusions SET name=?,enabled=?,participants=?,judge=?,max_tokens=? WHERE id=?`,
		f.Name, b2i(f.Enabled), string(blob), f.Judge, f.MaxTokens, f.ID)
	return err
}

func (s *Store) DeleteFusion(id int64) error {
	_, err := s.db.Exec(`DELETE FROM fusions WHERE id=?`, id)
	return err
}

func (s *Store) DeleteRouter(id int64) error {
	_, err := s.db.Exec(`DELETE FROM routers WHERE id=?`, id)
	return err
}

// ---- share endpoints (scoped keys for others) ----

type Endpoint struct {
	ID         int64  `json:"id"`
	Name       string `json:"name"`
	APIKey     string `json:"api_key"`     // decrypted; the shareable key
	Models     string `json:"models"`      // comma-sep allowed model ids/globs; "" = all
	ForceModel string `json:"force_model"` // if set, every request routes here (e.g. "auto")
	Enabled    bool   `json:"enabled"`
	Note       string `json:"note"`
	Caveman    bool   `json:"caveman"`
	NoFallback bool   `json:"no_fallback"`
	// ContextLength caps the context window this share advertises and enforces.
	// It only ever LOWERS the window derived from the upstream; a value above
	// what the backend reports is ignored. A field that can raise the window
	// would let a harness build prompts no slot can accept -- the exact failure
	// this exists to prevent. 0 means "no cap, use the derived value".
	ContextLength int `json:"context_length"`
	// ReasoningEffort / ReasoningForce: the thinking level for this share, same
	// semantics as the provider fields; the share setting wins over the
	// provider's when both are set.
	ReasoningEffort string `json:"reasoning_effort"`
	ReasoningForce  bool   `json:"reasoning_force"`
}

// ReasoningLevels are the accepted thinking levels, lowest first. "off"
// disables thinking where the dialect can express that; the others map to the
// OpenAI reasoning_effort vocabulary that llama.cpp, vLLM and the cloud
// providers all read.
var ReasoningLevels = []string{"off", "low", "medium", "high", "xhigh"}

// NormalizeReasoning validates a thinking level from the UI/CLI/API. "" means
// "not set"; "none" is accepted as a spelling of "off".
func NormalizeReasoning(v string) (string, error) {
	v = strings.ToLower(strings.TrimSpace(v))
	if v == "" {
		return "", nil
	}
	if v == "none" {
		v = "off"
	}
	for _, l := range ReasoningLevels {
		if v == l {
			return v, nil
		}
	}
	return "", fmt.Errorf("reasoning_effort %q: want one of %s", v, strings.Join(ReasoningLevels, "|"))
}

// Endpoints returns the cached share-endpoint list (see maybeReload). Keys
// are decrypted once per reload rather than on every /e/ request.
func (s *Store) Endpoints() ([]Endpoint, error) {
	s.maybeReload()
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Endpoint, len(s.endpoints))
	copy(out, s.endpoints)
	return out, nil
}

func (s *Store) loadEndpoints() ([]Endpoint, error) {
	rows, err := s.db.Query(`SELECT id,name,api_key_enc,models,force_model,enabled,note,caveman,no_fallback,context_length,reasoning_effort,reasoning_force FROM endpoints ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Endpoint
	for rows.Next() {
		var e Endpoint
		var enc []byte
		var en, cave, nofb, rforce int
		if err := rows.Scan(&e.ID, &e.Name, &enc, &e.Models, &e.ForceModel, &en, &e.Note, &cave, &nofb, &e.ContextLength, &e.ReasoningEffort, &rforce); err != nil {
			return nil, err
		}
		e.Enabled, e.Caveman, e.NoFallback = en == 1, cave == 1, nofb == 1
		e.ReasoningForce = rforce == 1
		e.APIKey, _ = s.decrypt(enc)
		out = append(out, e)
	}
	return out, rows.Err()
}

func (s *Store) EndpointByName(name string) (Endpoint, bool) {
	eps, _ := s.Endpoints()
	for _, e := range eps {
		if strings.EqualFold(e.Name, name) {
			return e, true
		}
	}
	return Endpoint{}, false
}

// SaveEndpoint persists e and refreshes the in-memory caches.
func (s *Store) SaveEndpoint(e *Endpoint) error {
	lvl, err := NormalizeReasoning(e.ReasoningEffort)
	if err != nil {
		return err
	}
	e.ReasoningEffort = lvl
	if err := s.saveEndpoint(e); err != nil {
		return err
	}
	return s.reload()
}

func (s *Store) saveEndpoint(e *Endpoint) error {
	e.Name = strings.TrimSpace(e.Name)
	if e.Name == "" {
		return errors.New("endpoint name is required")
	}
	if strings.ContainsAny(e.Name, "/ ?#") {
		return errors.New("endpoint name must be a plain slug (no spaces or / ? #)")
	}
	enc, err := s.encrypt(e.APIKey)
	if err != nil {
		return err
	}
	if e.ID == 0 {
		res, err := s.db.Exec(`INSERT INTO endpoints(name,api_key_enc,models,force_model,enabled,note,caveman,no_fallback,context_length,reasoning_effort,reasoning_force) VALUES(?,?,?,?,?,?,?,?,?,?,?)`,
			e.Name, enc, e.Models, e.ForceModel, b2i(e.Enabled), e.Note, b2i(e.Caveman), b2i(e.NoFallback), e.ContextLength, e.ReasoningEffort, b2i(e.ReasoningForce))
		if err != nil {
			return err
		}
		e.ID, _ = res.LastInsertId()
		return nil
	}
	if e.APIKey == "" { // keep existing key on blank
		_, err = s.db.Exec(`UPDATE endpoints SET name=?,models=?,force_model=?,enabled=?,note=?,caveman=?,no_fallback=?,context_length=?,reasoning_effort=?,reasoning_force=? WHERE id=?`,
			e.Name, e.Models, e.ForceModel, b2i(e.Enabled), e.Note, b2i(e.Caveman), b2i(e.NoFallback), e.ContextLength, e.ReasoningEffort, b2i(e.ReasoningForce), e.ID)
	} else {
		_, err = s.db.Exec(`UPDATE endpoints SET name=?,api_key_enc=?,models=?,force_model=?,enabled=?,note=?,caveman=?,no_fallback=?,context_length=?,reasoning_effort=?,reasoning_force=? WHERE id=?`,
			e.Name, enc, e.Models, e.ForceModel, b2i(e.Enabled), e.Note, b2i(e.Caveman), b2i(e.NoFallback), e.ContextLength, e.ReasoningEffort, b2i(e.ReasoningForce), e.ID)
	}
	return err
}

func (s *Store) DeleteEndpoint(id int64) error {
	_, err := s.db.Exec(`DELETE FROM endpoints WHERE id=?`, id)
	if err != nil {
		return err
	}
	return s.reload()
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

// SkillToken derives a read capability for one skill within one scope
// ("e:<endpoint>" or "p:<provider>") from the data-dir secret. It grants
// exactly that one GET, reveals neither the endpoint key nor the public key,
// and is deterministic so the catalog that carries it stays byte-stable in
// the cached prompt prefix.
func (s *Store) SkillToken(scope, skill string) string {
	mac := hmac.New(sha256.New, s.key)
	mac.Write([]byte("skill\x00" + strings.ToLower(scope) + "\x00" + strings.ToLower(skill)))
	return hex.EncodeToString(mac.Sum(nil))[:32]
}
