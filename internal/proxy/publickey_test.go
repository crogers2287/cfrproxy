package proxy

import (
	"net/http/httptest"
	"testing"
)

// The data-plane gate: keyless only for direct peers inside trusted_cidrs
// (RFC1918, loopback, Tailscale by default); everything else — a forwarded
// request, a public peer — must carry a public API key or admin credentials.
func TestPublicKeyGateTrustModel(t *testing.T) {
	s := newDiscoveryStore(t)
	s.SetSetting("public_api_keys", "cfr_pub")
	p := New(s)
	req := func(remote string, hdr map[string]string) bool {
		r := httptest.NewRequest("POST", "/v1/chat/completions", nil)
		r.RemoteAddr = remote
		for k, v := range hdr {
			r.Header.Set(k, v)
		}
		return p.publicKeyOK(r)
	}
	if !req("127.0.0.1:4000", nil) || !req("[::ffff:192.168.1.7]:4000", nil) || !req("100.91.28.112:4000", nil) {
		t.Fatal("LAN / loopback / Tailscale peers must stay keyless")
	}
	if req("203.0.113.9:4000", nil) {
		t.Fatal("a public peer without a key must be refused")
	}
	if !req("203.0.113.9:4000", map[string]string{"Authorization": "Bearer cfr_pub"}) {
		t.Fatal("a public peer with the key must pass")
	}
	if req("203.0.113.9:4000", map[string]string{"Authorization": "Bearer cfr_pu"}) {
		t.Fatal("a wrong key must be refused")
	}
	if req("192.168.1.5:4000", map[string]string{"X-Forwarded-For": "8.8.8.8"}) {
		t.Fatal("a forwarded request from the LAN reverse proxy must still need a key")
	}
	s.SetSetting("trusted_cidrs", "203.0.113.0/24")
	if !req("203.0.113.9:4000", nil) {
		t.Fatal("trusted_cidrs override should trust the peer")
	}
	if req("192.168.1.7:4000", nil) {
		t.Fatal("an explicit trusted_cidrs replaces the defaults")
	}
	s.SetSetting("public_api_keys", "")
	if req("198.51.100.2:4000", nil) {
		t.Fatal("no keys configured must not mean open to untrusted peers")
	}
}
