package proxy

// Routing trajectories: what the smart router actually did to each
// conversation, reconstructed from the trace table. Every smart-routed trace
// carries "auto→<tier>→<provider/model> … conv:<id>" in its note; grouping on
// the id gives one row per conversation with its model sequence, cache
// behaviour and escalations — the view "is auto routing working" needs.

import (
	"regexp"
	"sort"
	"strings"
	"time"
)

var trajRe = regexp.MustCompile(`auto→([a-z]+)(·sticky)?→([^ ]+).* conv:([0-9a-f]{8})`)

type TrajectoryHop struct {
	Model string `json:"model"`
	Turns int    `json:"turns"`
}

type Trajectory struct {
	Conv        string          `json:"conv"`
	FirstTS     int64           `json:"first_ts"`
	LastTS      int64           `json:"last_ts"`
	Turns       int             `json:"turns"`
	Tier        string          `json:"tier"`        // tier of the first turn
	Hops        []TrajectoryHop `json:"hops"`        // model sequence, in order
	Escalations int             `json:"escalations"` // model changes after the first turn
	PromptTok   int64           `json:"prompt_tokens"`
	CachedTok   int64           `json:"cached_tokens"`
	CacheHitPct float64         `json:"cache_hit_pct"`
	AvgLatMS    int64           `json:"avg_latency_ms"`
	MaxLatMS    int64           `json:"max_latency_ms"`
	Errors      int             `json:"errors"`
	KVX         string          `json:"kvx,omitempty"` // first turn's kvx verdict
	Inbound     string          `json:"inbound"`
}

// RouteTrajectories groups the newest `scan` traces (max 5000) into
// conversations, newest activity first, at most `limit` rows.
func (p *Proxy) RouteTrajectories(scan, limit int) ([]Trajectory, error) {
	if scan <= 0 || scan > 5000 {
		scan = 2000
	}
	if limit <= 0 {
		limit = 100
	}
	traces, err := p.Store.Traces(0, scan)
	if err != nil {
		return nil, err
	}
	byConv := map[string]*Trajectory{}
	// Traces come newest first; walk oldest → newest so hops are in order.
	for i := len(traces) - 1; i >= 0; i-- {
		t := traces[i]
		m := trajRe.FindStringSubmatch(t.Note)
		if m == nil {
			continue
		}
		tier, model, conv := m[1], m[3], m[4]
		tj := byConv[conv]
		if tj == nil {
			tj = &Trajectory{Conv: conv, FirstTS: t.TS, Tier: tier, Inbound: t.Inbound}
			if j := strings.Index(t.Note, "kvx→"); j >= 0 {
				k := t.Note[j:]
				if e := strings.IndexByte(k, ' '); e > 0 && !strings.HasPrefix(k, "kvx→miss") {
					k = k[:e]
				}
				tj.KVX = k
			}
			byConv[conv] = tj
		}
		tj.LastTS = t.TS
		tj.Turns++
		if n := len(tj.Hops); n > 0 && tj.Hops[n-1].Model == model {
			tj.Hops[n-1].Turns++
		} else {
			if n > 0 {
				tj.Escalations++
			}
			tj.Hops = append(tj.Hops, TrajectoryHop{Model: model, Turns: 1})
		}
		tj.PromptTok += int64(t.PromptTokens)
		tj.CachedTok += int64(t.CachedTokens)
		tj.AvgLatMS += t.LatencyMS
		if t.LatencyMS > tj.MaxLatMS {
			tj.MaxLatMS = t.LatencyMS
		}
		if t.Status >= 400 || (t.Err != "" && !strings.Contains(t.Err, "failover from")) {
			tj.Errors++
		}
	}
	out := make([]Trajectory, 0, len(byConv))
	for _, tj := range byConv {
		if tj.Turns > 0 {
			tj.AvgLatMS /= int64(tj.Turns)
		}
		if tj.PromptTok > 0 {
			tj.CacheHitPct = 100 * float64(tj.CachedTok) / float64(tj.PromptTok)
		}
		out = append(out, *tj)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].LastTS > out[j].LastTS })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// Text renders trajectories for the CLI.
func TrajectoriesText(ts []Trajectory) string {
	var b strings.Builder
	b.WriteString("conv      last      turns tier     cache  avg/max latency  route\n")
	for _, t := range ts {
		var hops []string
		for _, h := range t.Hops {
			if h.Turns > 1 {
				hops = append(hops, h.Model+"×"+itoa(h.Turns))
			} else {
				hops = append(hops, h.Model)
			}
		}
		extra := ""
		if t.Errors > 0 {
			extra += " errors=" + itoa(t.Errors)
		}
		if t.KVX != "" {
			extra += " " + t.KVX
		}
		b.WriteString(padRight(t.Conv, 10) + time.UnixMilli(t.LastTS).Local().Format("15:04:05") + "  " +
			padRight(itoa(t.Turns), 6) + padRight(t.Tier, 9) + padRight(itoa(int(t.CacheHitPct))+"%", 7) +
			padRight(itoa(int(t.AvgLatMS))+"/"+itoa(int(t.MaxLatMS))+"ms", 17) + strings.Join(hops, " → ") + extra + "\n")
	}
	return b.String()
}

func itoa(n int) string { return strings.TrimSpace(strings.Repeat(" ", 0) + fmtInt(n)) }
func fmtInt(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var d []byte
	for n > 0 {
		d = append([]byte{byte('0' + n%10)}, d...)
		n /= 10
	}
	if neg {
		d = append([]byte{'-'}, d...)
	}
	return string(d)
}
func padRight(s string, w int) string {
	for len(s) < w {
		s += " "
	}
	return s + " "
}
