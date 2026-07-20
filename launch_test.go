package main

import "testing"

func TestFuzzyModel(t *testing.T) {
	models := []string{"qwen-3.8-max-preview-thinking", "glm-5.2", "agents-a1", "agents-a1-q8"}
	cases := []struct {
		want    string
		expect  string
		matched bool
	}{
		{"glm-5.2", "glm-5.2", true},                              // exact
		{"GLM-5.2", "glm-5.2", true},                              // case-insensitive
		{"preview", "qwen-3.8-max-preview-thinking", true},        // unique substring
		{"Qwen3.8", "qwen-3.8-max-preview-thinking", true},        // punctuation-blind
		{"agents-a1", "agents-a1", true},                          // exact beats prefix ambiguity
		{"agents", "", false},                                     // ambiguous substring
		{"nope", "", false},                                       // no match
	}
	for _, c := range cases {
		got, ok := fuzzyModel(models, c.want)
		if ok != c.matched || got != c.expect {
			t.Errorf("fuzzyModel(%q) = %q,%v want %q,%v", c.want, got, ok, c.expect, c.matched)
		}
	}
}
