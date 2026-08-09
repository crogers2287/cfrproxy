package proxy

// Image generation.
//
// The chat path normalises every request through wire.Request — messages,
// tools, streaming deltas. An image request shares none of that shape
// (prompt/size/n/quality in, data[].b64_json out), so routing it through the
// translation layer would mean inventing a second normalised type for a
// payload every upstream already agrees on. OpenAI, xAI and CLIProxyAPI all
// speak the same /v1/images/generations contract.
//
// So this forwards the body untouched and rewrites exactly one field: the
// model id. That is the one thing cfrproxy genuinely owns, because it renames
// models per mount ("grok/grok-imagine-image" on the global list, bare
// "grok-imagine-image" under /p/grok). Everything else — size, quality,
// aspect_ratio, response_format, provider-specific extensions — reaches the
// provider exactly as the caller wrote it, so a new parameter needs no change
// here.
//
// Not included: /v1/images/edits. The OpenAI contract for edits is
// multipart/form-data carrying the source image, which needs a different send
// path (send() sets a JSON content type and cannot stream a multipart body).
// Half-supporting it would mean silently mangling uploads, so it is left out
// until it can be done properly.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/crogers2287/cfrproxy/internal/store"
)

// maxImageBody caps the request body. Generation payloads are small (a prompt
// plus options), so this is generous; it exists to stop an unbounded read.
const maxImageBody = 8 << 20 // 8 MiB

// imagesUpstreamPath is the provider-side path. Unlike chat, this does not vary
// by provider type: the OpenAI images contract is what every upstream cfrproxy
// fronts implements, and send() already strips the duplicate "/v1" for bases
// pasted in SDK convention.
const imagesUpstreamPath = "/v1/images/generations"

// handleImagesScoped resolves a /p/{provider} mount before delegating, so an
// unknown mount 404s here rather than surfacing as a confusing model-resolution
// failure further down.
func (p *Proxy) handleImagesScoped(w http.ResponseWriter, r *http.Request, scope string) {
	prov, ok := p.Store.ProviderByName(scope)
	if !ok {
		httpErr(w, "openai", 404, "unknown provider mount: "+scope)
		return
	}
	p.handleImages(w, r, prov.Name, nil)
}

// handleEndpointImages authenticates a share endpoint, then applies its model
// policy inside handleImages.
func (p *Proxy) handleEndpointImages(w http.ResponseWriter, r *http.Request) {
	ep, ok := p.authEndpoint(w, r, "openai")
	if !ok {
		return
	}
	p.handleImages(w, r, "", &ep)
}

// handleImages proxies an image-generation request to the provider that serves
// the requested model.
func (p *Proxy) handleImages(w http.ResponseWriter, r *http.Request, scope string, ep *store.Endpoint) {
	start := time.Now()

	body, err := io.ReadAll(io.LimitReader(r.Body, maxImageBody+1))
	if err != nil {
		httpErr(w, "openai", 400, "read body: "+err.Error())
		return
	}
	if len(body) > maxImageBody {
		httpErr(w, "openai", 413, fmt.Sprintf("image request body exceeds %d bytes", maxImageBody))
		return
	}

	var req struct {
		Model string `json:"model"`
	}
	// A malformed body is the provider's to reject — cfrproxy only needs the
	// model to pick one. On a scoped mount even that is optional.
	_ = json.Unmarshal(body, &req)

	reqModel := strings.TrimSpace(req.Model)
	if scope != "" {
		// Mirror the chat path: a scoped mount forces the provider, a bare id
		// is qualified to it, and a "provider/model" naming someone else is
		// corrected rather than honoured.
		m := p.stripProviderPrefix(reqModel)
		if m == "" {
			if prov, ok := p.Store.ProviderByName(scope); ok {
				m = prov.DefaultModel
			}
		}
		reqModel = scope + "/" + m
	}
	if strings.TrimSpace(strings.TrimSuffix(reqModel, "/")) == "" {
		httpErr(w, "openai", 400, `image request needs a "model" (or use the /p/{provider}/v1/images/generations mount)`)
		return
	}

	// Same share-endpoint policy the chat path applies: a forced model wins
	// outright, otherwise the requested one must be on the allow-list.
	if ep != nil {
		if ep.ForceModel != "" {
			reqModel = ep.ForceModel
		} else if !p.modelAllowed(*ep, reqModel) {
			httpErr(w, "openai", 403, "model not permitted on this endpoint: "+reqModel)
			return
		}
	}

	prov, model, err := p.ResolveModel(r.Context(), reqModel)
	if err != nil {
		httpErr(w, "openai", 503, err.Error())
		return
	}

	outBody, err := setJSONModel(body, model)
	if err != nil {
		httpErr(w, "openai", 400, "rewrite model: "+err.Error())
		return
	}

	tr := &store.Trace{
		TS: start.UnixMilli(), Provider: prov.Name, Model: model,
		Inbound: "images", ReqSnip: snip(outBody),
	}
	defer func() {
		tr.LatencyMS = time.Since(start).Milliseconds()
		p.Store.AddTrace(tr)
		p.Hub.Publish(*tr)
	}()

	resp, err := p.send(r.Context(), prov, imagesUpstreamPath, outBody)
	if err != nil {
		tr.Status, tr.Err = 502, err.Error()
		httpErr(w, "openai", 502, err.Error())
		return
	}
	defer resp.Body.Close()
	tr.Status = resp.StatusCode

	rb, err := io.ReadAll(resp.Body)
	if err != nil {
		tr.Status, tr.Err = 502, err.Error()
		httpErr(w, "openai", 502, "read upstream: "+err.Error())
		return
	}
	if resp.StatusCode >= 400 {
		tr.Err = string(snip(rb))
	}
	// Generated images come back base64 or as a URL; either way the response is
	// JSON and small enough to hand over whole. Only the snippet is stored —
	// a full b64 image would bloat the trace database for no diagnostic gain.
	tr.RespSnip = snip(rb)

	ct := resp.Header.Get("Content-Type")
	if ct == "" {
		ct = "application/json"
	}
	w.Header().Set("Content-Type", ct)
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(rb)
}

// setJSONModel replaces the top-level "model" field, preserving every other key
// exactly as sent. Round-tripping through a generic map loses key order, which
// no provider cares about, and is far safer than editing the raw JSON by hand.
func setJSONModel(body []byte, model string) ([]byte, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		// Not an object we can edit. The model was resolved from the scope, so
		// forward it untouched and let the provider validate.
		return body, nil
	}
	enc, err := json.Marshal(model)
	if err != nil {
		return nil, err
	}
	if m == nil {
		m = map[string]json.RawMessage{}
	}
	m["model"] = enc
	return json.Marshal(m)
}
