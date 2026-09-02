package api

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/crogers2287/cfrproxy/internal/store"
)

func authFixture(t *testing.T) http.Handler {
	t.Helper()
	dir, _ := os.MkdirTemp("", "cfrproxy-auth")
	t.Cleanup(func() { os.RemoveAll(dir) })
	s, err := store.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	a := &API{Store: s}
	s.SetSetting("admin_user", "admin")
	if err := a.SetPassword("pw"); err != nil {
		t.Fatal(err)
	}
	return a.basicAuth(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
}

func TestBasicAuthRejectsCrossSiteWrites(t *testing.T) {
	h := authFixture(t)
	do := func(method, ct, sfs string, auth bool) int {
		var body *strings.Reader
		if method == "GET" {
			body = strings.NewReader("")
		} else {
			body = strings.NewReader(`{"id":1,"base_url":"https://attacker.example"}`)
		}
		req := httptest.NewRequest(method, "/admin/api/providers/scan-models", body)
		if ct != "" {
			req.Header.Set("Content-Type", ct)
		}
		if sfs != "" {
			req.Header.Set("Sec-Fetch-Site", sfs)
		}
		if auth {
			req.SetBasicAuth("admin", "pw")
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec.Code
	}
	if got := do("GET", "", "", false); got != 401 {
		t.Fatalf("unauthenticated GET: want 401, got %d", got)
	}
	if got := do("GET", "", "", true); got != 204 {
		t.Fatalf("authenticated GET: want 204, got %d", got)
	}
	// the CSRF shape: a cross-site <form enctype=text/plain> carrying cached Basic creds
	if got := do("POST", "text/plain", "cross-site", true); got != 403 {
		t.Fatalf("cross-site text/plain POST: want 403, got %d", got)
	}
	if got := do("POST", "text/plain", "", true); got != 403 {
		t.Fatalf("non-JSON POST without Sec-Fetch-Site: want 403, got %d", got)
	}
	if got := do("POST", "application/json", "cross-site", true); got != 403 {
		t.Fatalf("cross-site JSON POST: want 403, got %d", got)
	}
	// what the WebUI and scripts actually send
	if got := do("POST", "application/json; charset=utf-8", "same-origin", true); got != 204 {
		t.Fatalf("same-origin JSON POST: want 204, got %d", got)
	}
	if got := do("POST", "application/json", "", true); got != 204 {
		t.Fatalf("non-browser JSON POST: want 204, got %d", got)
	}
}
