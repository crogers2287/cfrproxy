package proxy

// Caveman-style payload compression, opt-in per provider and per share
// endpoint (off by default). Inspired by github.com/JuliusBrussee/caveman:
// detect what a bulky payload IS, then apply a type-specific reduction that
// keeps the answer-critical parts (errors, structure, signatures, head/tail)
// and elides the repetitive middle.
//
// TWO RULES THIS IMPLEMENTATION WILL NOT BREAK, both learned the hard way on
// this box:
//
//  1. PREFIX-CACHE SAFETY. Compression is DETERMINISTIC and PER-MESSAGE: the
//     same message text always yields the same output, independent of the rest
//     of the conversation, the token budget, or how many turns have passed.
//     Upstream caveman packs context by BM25 relevance against a budget, which
//     re-compresses earlier messages differently as a conversation grows —
//     that rewrites the cached prefix every turn. Measured here: a stable
//     prefix is 34.6s cold vs 3.22s warm (92.7% hit). Losing the cache to save
//     ~30% of tokens is a large net loss, so budget-aware packing is
//     deliberately NOT implemented.
//
//  2. NEVER TOUCH THE STABLE HEAD. System prompts and tool schemas are the
//     cache prefix (and now carry preloaded skills). Only `tool`-role results
//     are compressed, and only those older than cavemanKeepRecent, because the
//     newest results are what the model is actively reasoning about.
//
// Everything is lossy by design but signposted: every elision leaves an
// explicit marker saying what was dropped, so the model knows the content was
// abridged rather than silently truncated.

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/crogers2287/cfrproxy/internal/wire"
)

// cavemanMarker tags every elision. Its second job is idempotency: a
// compressed tool result STAYS in the conversation and is re-sent on every
// later turn, so re-compressing it would shrink it a little more each time and
// the "stable" prefix would drift — silently destroying the prefix cache this
// design exists to protect. Content already bearing the marker is returned
// untouched.
const cavemanMarker = "[caveman:"

const (
	// Messages at or below this stay verbatim. Small results are already cheap
	// and compressing them risks losing the whole answer for no real saving.
	cavemanMinBytes = 2000
	// The newest N tool results are never compressed.
	cavemanKeepRecent = 2
)

// CavemanStats is what gets recorded on the trace.
type CavemanStats struct {
	Msgs   int // messages rewritten
	Before int // bytes before
	After  int // bytes after
}

// Saved returns bytes removed (never negative).
func (c CavemanStats) Saved() int {
	if c.Before <= c.After {
		return 0
	}
	return c.Before - c.After
}

type cavemanKind string

const (
	kindJSON cavemanKind = "json"
	kindDiff cavemanKind = "diff"
	kindLogs cavemanKind = "logs"
	kindCode cavemanKind = "code"
	kindText cavemanKind = "text"
)

// detectKind classifies a payload. Order matters: JSON and diff have strong
// unambiguous signals, logs and code are decided by line statistics.
func detectKind(s string) cavemanKind {
	t := strings.TrimSpace(s)
	if t == "" {
		return kindText
	}
	if (strings.HasPrefix(t, "{") && strings.HasSuffix(t, "}")) ||
		(strings.HasPrefix(t, "[") && strings.HasSuffix(t, "]")) {
		if json.Valid([]byte(t)) {
			return kindJSON
		}
	}
	lines := strings.Split(t, "\n")
	// Diff markers are unambiguous, so they are checked before the
	// line-count gate that the statistical detectors need.
	for _, l := range lines {
		if strings.HasPrefix(l, "diff --git") || strings.HasPrefix(l, "@@ ") {
			return kindDiff
		}
	}
	if len(lines) > 4 {
		var diffish, logish, codeish int
		for _, l := range lines {
			switch {
			case strings.HasPrefix(l, "diff --git"), strings.HasPrefix(l, "@@ "),
				strings.HasPrefix(l, "+++ "), strings.HasPrefix(l, "--- "):
				diffish++
			}
			if hasLogLevel(l) || hasTimestampPrefix(l) {
				logish++
			}
			ls := strings.TrimSpace(l)
			switch {
			case strings.HasPrefix(ls, "func "), strings.HasPrefix(ls, "def "),
				strings.HasPrefix(ls, "class "), strings.HasPrefix(ls, "import "),
				strings.HasPrefix(ls, "from "), strings.HasPrefix(ls, "package "),
				strings.HasPrefix(ls, "function "), strings.HasSuffix(ls, "{"):
				codeish++
			}
		}
		n := len(lines)
		if diffish >= 2 {
			return kindDiff
		}
		if logish*3 >= n { // a third or more of lines look like log records
			return kindLogs
		}
		if codeish*8 >= n {
			return kindCode
		}
	}
	return kindText
}

func hasLogLevel(l string) bool {
	for _, lv := range []string{"DEBUG", "INFO", "WARN", "WARNING", "ERROR", "FATAL", "TRACE"} {
		if strings.Contains(l, lv) {
			return true
		}
	}
	return false
}

// hasTimestampPrefix spots the common "2026-08-23 10:11:12" / "10:11:12" heads
// without pulling in a date parser.
func hasTimestampPrefix(l string) bool {
	l = strings.TrimSpace(l)
	if len(l) < 8 {
		return false
	}
	digits, seps := 0, 0
	for i := 0; i < 10 && i < len(l); i++ {
		switch {
		case l[i] >= '0' && l[i] <= '9':
			digits++
		case l[i] == '-' || l[i] == ':' || l[i] == '/':
			seps++
		}
	}
	return digits >= 5 && seps >= 2
}

// looksLikeSource is a loose check used only to decide whether to number lines.
// It is deliberately more permissive than detectKind's code branch: being wrong
// here costs a few characters, whereas missing it costs correct citations.
func looksLikeSource(lines []string) bool {
	hits := 0
	for _, l := range lines {
		t := strings.TrimSpace(l)
		switch {
		case strings.HasPrefix(t, "def "), strings.HasPrefix(t, "func "),
			strings.HasPrefix(t, "class "), strings.HasPrefix(t, "import "),
			strings.HasPrefix(t, "from "), strings.HasPrefix(t, "package "),
			strings.HasPrefix(t, "function "), strings.HasPrefix(t, "return "),
			strings.HasPrefix(t, "if "), strings.HasPrefix(t, "for "),
			strings.HasPrefix(t, "#include"), strings.HasSuffix(t, "{"),
			strings.HasSuffix(t, ":") && len(t) > 3:
			hits++
		}
		if hits >= 4 {
			return true
		}
	}
	return false
}

func elided(what string, n int) string {
	return fmt.Sprintf("\n… [caveman: elided %d %s] …\n", n, what)
}

// headTail keeps the first h and last t lines, marking what went missing.
// numbered=true prefixes each kept line with its original 1-based number so
// line references survive the elision (see compressCode).
func headTail(lines []string, h, t int, what string, numbered bool) string {
	if len(lines) <= h+t {
		return strings.Join(lines, "\n")
	}
	emit := func(idx int) string {
		if numbered {
			return fmt.Sprintf("%d| %s", idx+1, lines[idx])
		}
		return lines[idx]
	}
	var b strings.Builder
	for i := 0; i < h; i++ {
		if i > 0 {
			b.WriteString("\n")
		}
		b.WriteString(emit(i))
	}
	if numbered {
		b.WriteString(fmt.Sprintf("\n… [caveman: elided lines %d-%d] …\n", h+1, len(lines)-t))
	} else {
		b.WriteString(elided(what, len(lines)-h-t))
	}
	for i := len(lines) - t; i < len(lines); i++ {
		b.WriteString(emit(i))
		if i < len(lines)-1 {
			b.WriteString("\n")
		}
	}
	return b.String()
}

// compressLogs drops routine level lines and keeps errors plus the head/tail.
// Errors are what an agent actually needs out of a log dump.
func compressLogs(s string) string {
	lines := strings.Split(s, "\n")
	kept := make([]string, 0, len(lines))
	dropped := 0
	for i, l := range lines {
		keep := i < 5 || i >= len(lines)-5 // always keep the ends
		if !keep {
			up := strings.ToUpper(l)
			switch {
			case strings.Contains(up, "ERROR"), strings.Contains(up, "FATAL"),
				strings.Contains(up, "WARN"), strings.Contains(up, "EXCEPTION"),
				strings.Contains(up, "TRACEBACK"), strings.Contains(up, "PANIC"),
				strings.Contains(up, "FAIL"):
				keep = true
			}
		}
		if keep {
			if dropped > 0 {
				kept = append(kept, strings.TrimSuffix(strings.TrimPrefix(elided("routine log lines", dropped), "\n"), "\n"))
				dropped = 0
			}
			kept = append(kept, l)
			continue
		}
		dropped++
	}
	if dropped > 0 {
		kept = append(kept, strings.TrimSuffix(strings.TrimPrefix(elided("routine log lines", dropped), "\n"), "\n"))
	}
	return strings.Join(kept, "\n")
}

// compressCode keeps imports, declarations and signatures; elides bodies.
//
// Kept lines carry their ORIGINAL 1-based line number ("42| func foo() {").
// Without it, eliding a body renumbers everything below and any line reference
// the model produces is silently wrong — which makes compression unusable for
// the most common research ask ("find the bug, cite the line"). Numbering costs
// a few characters per kept line and buys correct citations; the elision
// markers name the dropped range for the same reason.
func compressCode(s string) string {
	lines := strings.Split(s, "\n")
	kept := make([]string, 0, len(lines))
	dropped, dropStart := 0, 0
	flush := func() {
		if dropped > 0 {
			kept = append(kept, fmt.Sprintf("… [caveman: elided lines %d-%d] …",
				dropStart, dropStart+dropped-1))
			dropped = 0
		}
	}
	for i, l := range lines {
		ls := strings.TrimSpace(l)
		keep := i < 3 || i >= len(lines)-3 || ls == ""
		if !keep {
			switch {
			case strings.HasPrefix(ls, "func "), strings.HasPrefix(ls, "def "),
				strings.HasPrefix(ls, "class "), strings.HasPrefix(ls, "type "),
				strings.HasPrefix(ls, "import "), strings.HasPrefix(ls, "from "),
				strings.HasPrefix(ls, "package "), strings.HasPrefix(ls, "function "),
				strings.HasPrefix(ls, "//"), strings.HasPrefix(ls, "#"),
				strings.HasPrefix(ls, "export "), strings.HasPrefix(ls, "const "):
				keep = true
			}
		}
		if keep {
			flush()
			kept = append(kept, fmt.Sprintf("%d| %s", i+1, l))
			continue
		}
		if dropped == 0 {
			dropStart = i + 1
		}
		dropped++
	}
	flush()
	return strings.Join(kept, "\n")
}

// compressDiff keeps hunk headers and changed lines, elides context runs.
func compressDiff(s string) string {
	lines := strings.Split(s, "\n")
	kept := make([]string, 0, len(lines))
	dropped := 0
	flush := func() {
		if dropped > 0 {
			kept = append(kept, strings.TrimSuffix(strings.TrimPrefix(elided("unchanged context lines", dropped), "\n"), "\n"))
			dropped = 0
		}
	}
	for _, l := range lines {
		keep := false
		switch {
		case strings.HasPrefix(l, "diff --git"), strings.HasPrefix(l, "@@"),
			strings.HasPrefix(l, "+++"), strings.HasPrefix(l, "---"),
			strings.HasPrefix(l, "+"), strings.HasPrefix(l, "-"),
			strings.HasPrefix(l, "index "), strings.HasPrefix(l, "new file"),
			strings.HasPrefix(l, "deleted file"), strings.HasPrefix(l, "rename "):
			keep = true
		}
		if keep {
			flush()
			kept = append(kept, l)
			continue
		}
		dropped++
	}
	flush()
	return strings.Join(kept, "\n")
}

// compressJSON truncates long arrays and long string values, preserving shape
// so the model still sees the schema it is reasoning about.
func compressJSON(s string) string {
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		return compressText(s)
	}
	out := shrinkJSON(v, 0)
	b, err := json.Marshal(out)
	if err != nil {
		return compressText(s)
	}
	return string(b)
}

const (
	jsonMaxArray  = 8   // keep this many elements per array
	jsonMaxString = 600 // truncate string values beyond this
	jsonMaxDepth  = 12
)

func shrinkJSON(v any, depth int) any {
	if depth > jsonMaxDepth {
		return "… [caveman: depth limit] …"
	}
	switch t := v.(type) {
	case []any:
		if len(t) > jsonMaxArray {
			out := make([]any, 0, jsonMaxArray+1)
			for _, e := range t[:jsonMaxArray] {
				out = append(out, shrinkJSON(e, depth+1))
			}
			out = append(out, fmt.Sprintf("… [caveman: elided %d more of %d array items] …", len(t)-jsonMaxArray, len(t)))
			return out
		}
		out := make([]any, 0, len(t))
		for _, e := range t {
			out = append(out, shrinkJSON(e, depth+1))
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, e := range t {
			out[k] = shrinkJSON(e, depth+1)
		}
		return out
	case string:
		if len(t) > jsonMaxString {
			return t[:jsonMaxString] + fmt.Sprintf("… [caveman: +%d chars]", len(t)-jsonMaxString)
		}
		return t
	default:
		return v
	}
}

// compressText is the fallback: keep a generous head and tail.
//
// It numbers lines when the payload looks like source. Real files often miss
// the code detector (a module of mostly assignments has few def/import lines),
// and losing line references on what is plainly code is worse than the few
// characters numbering costs.
func compressText(s string) string {
	lines := strings.Split(s, "\n")
	if len(lines) > 40 {
		return headTail(lines, 25, 10, "lines", looksLikeSource(lines))
	}
	// one enormous line (minified blob) — cut the middle
	if len(s) > 4000 {
		return s[:2500] + elided("characters", len(s)-3200) + s[len(s)-700:]
	}
	return s
}

// cavemanCompressOne applies the type-appropriate reduction. Exported shape is
// deterministic: same input, same output, always.
func cavemanCompressOne(s string) string {
	if strings.Contains(s, cavemanMarker) {
		return s // already compressed — see cavemanMarker
	}
	switch detectKind(s) {
	case kindJSON:
		return compressJSON(s)
	case kindDiff:
		return compressDiff(s)
	case kindLogs:
		return compressLogs(s)
	case kindCode:
		return compressCode(s)
	default:
		return compressText(s)
	}
}

// CavemanCompress rewrites bulky message content in place and reports what it
// saved. It never touches System or tool schemas — see the header for why.
//
// `explicit` distinguishes the two ways compression gets asked for, because
// they warrant different aggression:
//
//   - false — a standing provider/endpoint checkbox. Only `tool` results are
//     touched, so an ongoing conversation's prefix stays byte-stable and the
//     provider's prompt cache keeps paying off.
//   - true — the caller set X-Caveman on THIS call. Large `user` messages are
//     compressed too: someone piping a 30 KB log in as a user message plainly
//     means "shrink this", and a one-shot research call has no prefix cache to
//     protect. This is what the /caveman skill uses.
//
// The LAST message is never compressed under either policy: it carries the
// actual instruction, and truncating the question to save tokens on the
// haystack would be a bad trade.
func CavemanCompress(req *wire.Request, explicit bool) CavemanStats {
	if req == nil {
		return CavemanStats{}
	}
	// Index of the tool messages, so "keep the newest N" is well defined.
	toolIdx := make([]int, 0, len(req.Messages))
	for i, m := range req.Messages {
		if m.Role == "tool" {
			toolIdx = append(toolIdx, i)
		}
	}
	if len(toolIdx) == 0 && !explicit {
		return CavemanStats{}
	}
	// Under the explicit policy every tool result is fair game. The
	// keep-the-newest-N rule protects conversational quality under a standing
	// config, but in an AGENT LOOP the freshly-fetched result is always the
	// newest — so that rule meant the single biggest payload in the loop
	// (a whole file the agent just read) never got compressed, which is the
	// exact traffic this feature exists for. Measured: a 90 KB log read went
	// upstream at 32k tokens with the rule on.
	keepRecent := cavemanKeepRecent
	if explicit {
		keepRecent = 0
	}
	cutoff := len(toolIdx) - keepRecent
	last := len(req.Messages) - 1
	var st CavemanStats

	try := func(i int) {
		// Never compress a trailing USER message: that is the instruction, and
		// truncating the question to save tokens on the haystack is a bad
		// trade. A trailing TOOL result is different — in an agent loop it is
		// the payload, not the ask — so it stays compressible.
		if i == last && req.Messages[i].Role == "user" {
			return
		}
		orig := req.Messages[i].Content
		if len(orig) <= cavemanMinBytes {
			return
		}
		got := cavemanCompressOne(orig)
		if len(got) >= len(orig) {
			return // never grow a message
		}
		req.Messages[i].Content = got
		st.Msgs++
		st.Before += len(orig)
		st.After += len(got)
	}

	for n, i := range toolIdx {
		if n >= cutoff {
			break // newest results stay verbatim
		}
		try(i)
	}
	if explicit {
		for i, m := range req.Messages {
			if m.Role == "user" {
				try(i)
			}
		}
	}
	return st
}

// ── Per-request mode ──────────────────────────────────────────────────────
//
// The provider/endpoint checkboxes are a standing policy. A single call can
// override them, which is what the /caveman research skill uses: it asks for
// compression explicitly rather than needing the operator to flip a provider
// setting first.
//
//	Header:  X-Caveman: off | in | out | both      (also 1/true = both, 0/false = off)
//	Body:    {"caveman": "both"}  or  {"caveman": true}
//
// The body form is STRIPPED before the request is forwarded, so upstreams
// never see a parameter they would reject.

type CavemanMode string

const (
	CMOff  CavemanMode = "off"
	CMIn   CavemanMode = "in"  // compress the request we send upstream
	CMOut  CavemanMode = "out" // compress the reply we hand back
	CMBoth CavemanMode = "both"
)

func (m CavemanMode) CompressIn() bool  { return m == CMIn || m == CMBoth }
func (m CavemanMode) CompressOut() bool { return m == CMOut || m == CMBoth }

// ParseCavemanMode maps a user-supplied string to a mode. Unknown values are
// treated as off: a typo must never silently mangle a payload.
func ParseCavemanMode(v string) (CavemanMode, bool) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "off", "0", "false", "no", "none":
		return CMOff, true
	case "in", "input", "request", "req":
		return CMIn, true
	case "out", "output", "response", "resp":
		return CMOut, true
	case "both", "1", "true", "yes", "on", "all":
		return CMBoth, true
	case "":
		return CMOff, false
	default:
		return CMOff, false
	}
}

// CavemanModeFor resolves the effective mode. Request-level wins over the
// share endpoint, which wins over the provider. The stored booleans mean "in"
// only: output compression changes what the CALLER receives, so it is never
// switched on by a standing config checkbox — it must be asked for per call.
func CavemanModeFor(reqMode CavemanMode, reqSet, epOn, provOn bool) CavemanMode {
	if reqSet {
		return reqMode
	}
	if epOn || provOn {
		return CMIn
	}
	return CMOff
}

// CavemanCompressResponse shrinks an assistant reply. Used for CMOut/CMBoth,
// typically when a sub-agent's verbose answer is about to be pasted into a
// parent context. Tool-call arguments are left alone — they are executed, not
// read, and truncating them would produce malformed calls.
func CavemanCompressResponse(resp *wire.Response) CavemanStats {
	if resp == nil || len(resp.Content) <= cavemanMinBytes {
		return CavemanStats{}
	}
	before := resp.Content
	got := cavemanCompressOne(before)
	if len(got) >= len(before) {
		return CavemanStats{}
	}
	resp.Content = got
	return CavemanStats{Msgs: 1, Before: len(before), After: len(got)}
}
