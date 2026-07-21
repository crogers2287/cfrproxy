package api

// OAuth account management: cfrproxy's WebUI drives CLIProxyAPI's OAuth
// flows (Claude, Codex, Antigravity, Kimi, xAI/SuperGrok) through its
// management API. Device-flow providers (xai, kimi) show a user code; the
// browser-flow providers complete via callback paste-back. Settings:
//   cliproxy_mgmt_url (default http://127.0.0.1:8317)
//   cliproxy_mgmt_key (management secret, plaintext)

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

var oauthProviders = map[string]string{
	"claude":      "anthropic-auth-url",
	"anthropic":   "anthropic-auth-url",
	"codex":       "codex-auth-url",
	"antigravity": "antigravity-auth-url",
	"kimi":        "kimi-auth-url",
	"supergrok":   "xai-auth-url",
	"xai":         "xai-auth-url",
	"grok":        "xai-auth-url",
}

func (a *API) mgmtURL() string {
	if v := a.Store.Setting("cliproxy_mgmt_url"); v != "" {
		return strings.TrimRight(v, "/")
	}
	return "http://127.0.0.1:8317"
}

// mgmt performs an authenticated call against the CLIProxyAPI management API
// and returns the raw response body.
func (a *API) mgmt(method, path string, body io.Reader, contentType string) (int, []byte, error) {
	key := a.Store.Setting("cliproxy_mgmt_key")
	if key == "" {
		return 0, nil, fmt.Errorf("cliproxy_mgmt_key not configured — set it with: cfrproxy config set cliproxy_mgmt_key <key>")
	}
	req, err := http.NewRequest(method, a.mgmtURL()+"/v0/management"+path, body)
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	rb, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	return resp.StatusCode, rb, nil
}

func (a *API) registerOAuth(mux *http.ServeMux) {
	mux.HandleFunc("GET /admin/api/oauth/accounts", a.hOAuthAccounts)
	mux.HandleFunc("DELETE /admin/api/oauth/accounts", a.hOAuthAccountDelete)
	mux.HandleFunc("PATCH /admin/api/oauth/accounts/status", a.hOAuthAccountStatus)
	mux.HandleFunc("POST /admin/api/oauth/{provider}/start", a.hOAuthStart)
	mux.HandleFunc("GET /admin/api/oauth/status", a.hOAuthStatus)
	mux.HandleFunc("POST /admin/api/oauth/callback", a.hOAuthCallback)
	mux.HandleFunc("POST /admin/api/oauth/cancel", a.hOAuthCancel)
}

func (a *API) proxyMgmt(w http.ResponseWriter, method, path string, body io.Reader, ct string) {
	code, rb, err := a.mgmt(method, path, body, ct)
	if err != nil {
		httpErr(w, 502, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	w.Write(rb)
}

func (a *API) hOAuthAccounts(w http.ResponseWriter, r *http.Request) {
	a.proxyMgmt(w, "GET", "/auth-files", nil, "")
}

func (a *API) hOAuthAccountDelete(w http.ResponseWriter, r *http.Request) {
	name := r.URL.Query().Get("name")
	if name == "" {
		httpErr(w, 400, "name required")
		return
	}
	a.proxyMgmt(w, "DELETE", "/auth-files?name="+url.QueryEscape(name), nil, "")
}

func (a *API) hOAuthAccountStatus(w http.ResponseWriter, r *http.Request) {
	b, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	a.proxyMgmt(w, "PATCH", "/auth-files/status", strings.NewReader(string(b)), "application/json")
}

func (a *API) hOAuthStart(w http.ResponseWriter, r *http.Request) {
	prov := strings.ToLower(r.PathValue("provider"))
	ep, ok := oauthProviders[prov]
	if !ok {
		httpErr(w, 400, fmt.Sprintf("unknown oauth provider %q (want claude|codex|antigravity|kimi|supergrok)", prov))
		return
	}
	a.proxyMgmt(w, "GET", "/"+ep, nil, "")
}

func (a *API) hOAuthStatus(w http.ResponseWriter, r *http.Request) {
	state := r.URL.Query().Get("state")
	if state == "" {
		httpErr(w, 400, "state required")
		return
	}
	a.proxyMgmt(w, "GET", "/get-auth-status?state="+url.QueryEscape(state), nil, "")
}

// hOAuthCallback completes a browser-flow login: the user pastes the full
// localhost redirect URL they landed on; we forward it to CLIProxyAPI.
func (a *API) hOAuthCallback(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Provider    string `json:"provider"`
		State       string `json:"state"`
		RedirectURL string `json:"redirect_url"`
		Code        string `json:"code"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		httpErr(w, 400, err.Error())
		return
	}
	b, _ := json.Marshal(in)
	a.proxyMgmt(w, "POST", "/oauth-callback", strings.NewReader(string(b)), "application/json")
}

func (a *API) hOAuthCancel(w http.ResponseWriter, r *http.Request) {
	state := r.URL.Query().Get("state")
	if state == "" {
		httpErr(w, 400, "state required")
		return
	}
	a.proxyMgmt(w, "DELETE", "/oauth-session?state="+url.QueryEscape(state), nil, "")
}
