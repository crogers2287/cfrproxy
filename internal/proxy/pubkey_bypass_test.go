package proxy

import (
	"net/http/httptest"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

// publicKeyOK gates data-plane traffic that arrived through the reverse proxy.
// It accepts either a configured public_api_keys value OR valid admin
// BasicAuth — the latter exists so the WebUI's model scanner
// (/p/<provider>/v1/models?all=1, which is NOT under /admin/) keeps working
// once the gate is enabled. Verify both, and that near-misses are rejected.
func TestPublicKeyOKAdminBypass(t *testing.T) {
	st := newDiscoveryStore(t)
	hash, err := bcrypt.GenerateFromPassword([]byte("s3cret"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range map[string]string{
		"admin_user":      "admin",
		"admin_pass_hash": string(hash),
		"public_api_keys": "cfr_good",
	} {
		if err := st.SetSetting(k, v); err != nil {
			t.Fatal(err)
		}
	}
	p := &Proxy{Store: st}

	cases := []struct {
		name            string
		user, pass, key string
		fwd             bool
		want            bool
	}{
		{name: "direct connection is always allowed", fwd: false, want: true},
		{name: "via proxy, no creds", fwd: true, want: false},
		{name: "via proxy, correct api key", key: "cfr_good", fwd: true, want: true},
		{name: "via proxy, wrong api key", key: "cfr_bad", fwd: true, want: false},
		{name: "via proxy, correct admin creds", user: "admin", pass: "s3cret", fwd: true, want: true},
		{name: "via proxy, wrong admin password", user: "admin", pass: "nope", fwd: true, want: false},
		{name: "via proxy, wrong admin user", user: "root", pass: "s3cret", fwd: true, want: false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", "/v1/chat/completions", nil)
			if c.fwd {
				r.Header.Set("X-Forwarded-For", "203.0.113.9")
			}
			if c.key != "" {
				r.Header.Set("x-api-key", c.key)
			}
			if c.user != "" {
				r.SetBasicAuth(c.user, c.pass)
			}
			if got := p.publicKeyOK(r); got != c.want {
				t.Fatalf("publicKeyOK = %v, want %v", got, c.want)
			}
		})
	}
}
