// Package transform applies declarative JSON rewrite rules to request and
// response bodies at the dialect boundary. Rules are stored as JSON arrays:
//
//	[
//	  {"op":"set",     "path":"temperature", "value":0.2},
//	  {"op":"default", "path":"max_tokens",  "value":4096},
//	  {"op":"rename",  "path":"max_tokens",  "to":"max_completion_tokens"},
//	  {"op":"delete",  "path":"stream_options"}
//	]
//
// Paths are dot-separated object keys ("a.b.c"). Numeric segments index
// arrays ("choices.0.message").
package transform

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
)

type Rule struct {
	Op    string          `json:"op"` // set | default | rename | delete
	Path  string          `json:"path"`
	To    string          `json:"to,omitempty"`
	Value json.RawMessage `json:"value,omitempty"`
}

func Parse(raw json.RawMessage) ([]Rule, error) {
	var rules []Rule
	if err := json.Unmarshal(raw, &rules); err != nil {
		return nil, fmt.Errorf("invalid rules: %w", err)
	}
	for i, r := range rules {
		switch r.Op {
		case "set", "default":
			if r.Path == "" || len(r.Value) == 0 {
				return nil, fmt.Errorf("rule %d: %s needs path and value", i, r.Op)
			}
		case "rename":
			if r.Path == "" || r.To == "" {
				return nil, fmt.Errorf("rule %d: rename needs path and to", i)
			}
		case "delete":
			if r.Path == "" {
				return nil, fmt.Errorf("rule %d: delete needs path", i)
			}
		default:
			return nil, fmt.Errorf("rule %d: unknown op %q", i, r.Op)
		}
	}
	return rules, nil
}

// Apply runs rules over a JSON body and returns the rewritten body. A body
// that isn't a JSON object is returned unchanged.
func Apply(body []byte, rules []Rule) []byte {
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		return body
	}
	for _, r := range rules {
		applyOne(doc, r)
	}
	out, err := json.Marshal(doc)
	if err != nil {
		return body
	}
	return out
}

func applyOne(doc map[string]any, r Rule) {
	segs := strings.Split(r.Path, ".")
	parent, key, ok := walk(doc, segs)
	if !ok {
		if r.Op == "set" || r.Op == "default" {
			// create missing intermediate objects for set/default
			parent = mkpath(doc, segs[:len(segs)-1])
			if parent == nil {
				return
			}
			key = segs[len(segs)-1]
		} else {
			return
		}
	}
	switch r.Op {
	case "set":
		var v any
		json.Unmarshal(r.Value, &v)
		setKey(parent, key, v)
	case "default":
		if !hasKey(parent, key) {
			var v any
			json.Unmarshal(r.Value, &v)
			setKey(parent, key, v)
		}
	case "rename":
		if m, ok := parent.(map[string]any); ok {
			if v, exists := m[key]; exists {
				delete(m, key)
				m[r.To] = v
			}
		}
	case "delete":
		if m, ok := parent.(map[string]any); ok {
			delete(m, key)
		}
	}
}

// walk returns the parent container and final key for a path, ok=false if any
// intermediate segment is missing.
func walk(doc map[string]any, segs []string) (parent any, key string, ok bool) {
	var cur any = doc
	for i, seg := range segs {
		last := i == len(segs)-1
		if last {
			return cur, seg, true
		}
		switch c := cur.(type) {
		case map[string]any:
			next, exists := c[seg]
			if !exists {
				return nil, "", false
			}
			cur = next
		case []any:
			idx, err := strconv.Atoi(seg)
			if err != nil || idx < 0 || idx >= len(c) {
				return nil, "", false
			}
			cur = c[idx]
		default:
			return nil, "", false
		}
	}
	return nil, "", false
}

func mkpath(doc map[string]any, segs []string) any {
	var cur any = doc
	for _, seg := range segs {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		next, exists := m[seg]
		if !exists {
			nm := map[string]any{}
			m[seg] = nm
			cur = nm
			continue
		}
		cur = next
	}
	return cur
}

func hasKey(parent any, key string) bool {
	switch c := parent.(type) {
	case map[string]any:
		_, ok := c[key]
		return ok
	case []any:
		idx, err := strconv.Atoi(key)
		return err == nil && idx >= 0 && idx < len(c)
	}
	return false
}

func setKey(parent any, key string, v any) {
	switch c := parent.(type) {
	case map[string]any:
		c[key] = v
	case []any:
		if idx, err := strconv.Atoi(key); err == nil && idx >= 0 && idx < len(c) {
			c[idx] = v
		}
	}
}
