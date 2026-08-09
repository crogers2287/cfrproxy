package proxy

import "encoding/json"

// ProviderFallback gates the PER-PROVIDER failover chain — the `fallback`
// field on each provider row, followed transitively up to 3 hops in
// ServeHTTP.
//
// That chain predates the global chain and overlaps with it: a request could
// leave its addressed provider via the provider's own `fallback`, land
// somewhere never named in global_fallback, and only then fall into the global
// list. Two independent chains made the effective route hard to predict from
// the admin UI, which shows the global one. Concretely, on 2026-07-31 the
// `fred` provider carried `fallback = Deepseek/deepseek-v4-pro`, so while
// llama-swap was down every local `fred/*` request quietly billed DeepSeek —
// a provider that appeared in no visible routing config.
//
// Default is DISABLED (the zero value), so an absent setting means the global
// chain alone decides failover. Providers keep their `fallback` values in the
// DB untouched; flipping Enabled back on restores the previous behaviour
// exactly, with no re-entry of per-provider hops.
type ProviderFallback struct {
	Enabled bool `json:"enabled"`
}

// ProviderFallbackConfig reads the `provider_fallback` setting. An unset or
// unparseable value yields the zero value (disabled), matching the documented
// default rather than silently re-enabling a second routing path.
func (p *Proxy) ProviderFallbackConfig() ProviderFallback {
	var c ProviderFallback
	if raw := p.Store.Setting("provider_fallback"); raw != "" {
		json.Unmarshal([]byte(raw), &c)
	}
	return c
}
