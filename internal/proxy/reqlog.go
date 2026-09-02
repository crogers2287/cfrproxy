package proxy

import (
	"io"
	"log/slog"
	"net/http"
	"os"
	"strings"

	"github.com/crogers2287/cfrproxy/internal/store"
)

// Log carries one line per proxied request to stderr, which journald
// collects under the systemd unit. Before this the only record of a request
// was its trace row: a failure that never reached the trace table (or a
// locked DB) was invisible, and "why did Hermes get an error" meant opening
// SQLite. CFRPROXY_LOG=off silences it; CFRPROXY_LOG=json switches format.
var Log = newLogger()

func newLogger() *slog.Logger {
	switch os.Getenv("CFRPROXY_LOG") {
	case "off":
		return slog.New(slog.NewTextHandler(io.Discard, nil))
	case "json":
		return slog.New(slog.NewJSONHandler(os.Stderr, nil))
	}
	return slog.New(slog.NewTextHandler(os.Stderr, nil))
}

// logTrace writes the request line (r is nil for a failed-candidate trace). Failures and reroutes log at Warn so
// `journalctl -p warning` shows only what needs attention; a client that
// walked away mid-stream is not one of those.
func logTrace(r *http.Request, tr *store.Trace) {
	attrs := []any{}
	if r != nil {
		attrs = append(attrs, "method", r.Method, "path", r.URL.Path)
	}
	attrs = append(attrs,
		"client", tr.Client,
		"model", tr.Model, "provider", tr.Provider, "status", tr.Status,
		"ms", tr.LatencyMS, "stream", tr.Stream,
		"in", tr.PromptTokens, "out", tr.CompletionTokens, "cached", tr.CachedTokens,
	)
	if tr.Note != "" {
		attrs = append(attrs, "note", tr.Note)
	}
	if tr.Err != "" {
		attrs = append(attrs, "err", tr.Err)
	}
	if tr.Status >= 400 || (tr.Err != "" && !strings.HasPrefix(tr.Err, "stream aborted")) {
		Log.Warn("request", attrs...)
		return
	}
	Log.Info("request", attrs...)
}
