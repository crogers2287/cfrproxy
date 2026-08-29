// Package integrity implements a native, observation-only port of SIMURG's
// streaming decode-corruption signals. Runtime inference stays inside the
// cfrproxy Go binary; the upstream Python project remains useful for offline
// calibration after cfrproxy has collected representative traffic.
//
// The algorithms in this package are derived from SIMURG by Farid Aghayev and
// Elturan Ahmadbayli, licensed under Apache-2.0. See THIRD_PARTY_NOTICES.md and
// licenses/SIMURG-APACHE-2.0.txt.
package integrity

import (
	"bytes"
	"compress/zlib"
	"encoding/binary"
	"math"
	"math/bits"
	"regexp"
	"strings"
	"unicode"

	"golang.org/x/crypto/blake2b"
)

const (
	tailWindow     = 600
	surpriseWindow = 200
)

type featureSnapshot struct {
	DigitFrac         float64 `json:"digit_frac"`
	ForeignFrac       float64 `json:"foreign_frac"`
	RepeatRate        float64 `json:"repeat_rate"`
	ZlibRatio         float64 `json:"zlib_ratio"`
	TTR               float64 `json:"ttr"`
	ScriptSwitchRate  float64 `json:"script_switch_rate"`
	StructuralDensity float64 `json:"structural_density"`
	SymbolFrac        float64 `json:"symbol_frac"`
	MaxCharRun        float64 `json:"max_char_run"`
	SpaceFrac         float64 `json:"space_frac"`
	SurpriseZ         float64 `json:"surprise_z"`
	SurpriseLowFrac   float64 `json:"surprise_low_frac"`
	SimHashDrift      float64 `json:"simhash_drift"`
	Entropy           float64 `json:"entropy"`
	MaxShingleCount   float64 `json:"max_shingle_count"`
}

func (s featureSnapshot) mapValue() map[string]float64 {
	return map[string]float64{
		"digit_frac": s.DigitFrac, "foreign_frac": s.ForeignFrac,
		"repeat_rate": s.RepeatRate, "zlib_ratio": s.ZlibRatio,
		"ttr": s.TTR, "script_switch_rate": s.ScriptSwitchRate,
		"structural_density": s.StructuralDensity, "symbol_frac": s.SymbolFrac,
		"max_char_run": s.MaxCharRun, "space_frac": s.SpaceFrac,
		"surprise_z": s.SurpriseZ, "surprise_low_frac": s.SurpriseLowFrac,
		"simhash_drift": s.SimHashDrift, "entropy": s.Entropy,
		"max_shingle_count": s.MaxShingleCount,
	}
}

type streamFeatures struct {
	total          int
	tail           []rune
	switches       int
	lastScript     string
	runChar        rune
	runLen         int
	maxRun         int
	zlibRatio      float64
	sinceZlib      int
	lm             *onlineCharNGram
	repetition     *repetitionTracker
	simhash        *rollingSimHash
	surpriseBase   robustEWMA
	surpriseWindow []float64
	word           []rune
}

func newStreamFeatures() *streamFeatures {
	return &streamFeatures{
		zlibRatio:    1,
		lm:           newOnlineCharNGram(),
		repetition:   newRepetitionTracker(),
		simhash:      newRollingSimHash(48),
		surpriseBase: robustEWMA{alpha: 0.03},
	}
}

func (f *streamFeatures) feedRune(r rune) {
	f.total++

	surprise := f.lm.feed(r)
	f.surpriseWindow = append(f.surpriseWindow, surprise)
	if len(f.surpriseWindow) > surpriseWindow {
		f.surpriseWindow = f.surpriseWindow[len(f.surpriseWindow)-surpriseWindow:]
	}
	if f.total <= 300 {
		f.surpriseBase.update(surprise)
	}

	f.tail = append(f.tail, r)
	if len(f.tail) > tailWindow {
		f.tail = f.tail[len(f.tail)-tailWindow:]
	}

	class, script := classifyRune(r)
	if script != "" {
		if f.lastScript != "" && script != f.lastScript {
			f.switches++
		}
		f.lastScript = script
	}

	if r == f.runChar && !runExcluded(r) {
		f.runLen++
	} else {
		f.runChar, f.runLen = r, 1
	}
	if f.runLen > f.maxRun {
		f.maxRun = f.runLen
	}

	f.repetition.push(f.tail)
	if unicode.IsLetter(r) || unicode.IsDigit(r) {
		f.word = append(f.word, unicode.ToLower(r))
	} else if class == "space" || class == "symbol" {
		f.flushWord()
	}

	f.sinceZlib++
	if f.sinceZlib >= 200 {
		f.sinceZlib = 0
		f.zlibRatio = compressionRatio(string(f.tail))
	}
}

func (f *streamFeatures) flushWord() {
	if len(f.word) == 0 {
		return
	}
	f.simhash.push(string(f.word))
	f.word = f.word[:0]
}

func (f *streamFeatures) freezeBaseline() {
	f.surpriseBase.freeze()
	f.flushWord()
	f.simhash.setBaseline()
}

func (f *streamFeatures) snapshot(profile string) featureSnapshot {
	counts := map[string]int{}
	charCounts := map[rune]int{}
	letters, foreign := 0, 0
	for _, r := range f.tail {
		class, script := classifyRune(r)
		counts[class]++
		charCounts[r]++
		if script != "" {
			letters++
			if profile != ProfileMultilingual && script != "latin" && script != "cyrillic" {
				foreign++
			}
		}
	}
	n := maxInt(1, len(f.tail))
	tail := string(f.tail)
	words := wordsIn(tail)
	unique := map[string]bool{}
	for _, word := range words {
		unique[word] = true
	}
	structHits := 0
	for _, re := range structuralPatterns {
		structHits += len(re.FindAllStringIndex(tail, -1))
	}

	curSurprise := mean(f.surpriseWindow)
	lowThreshold := math.Max(0.5, f.surpriseBase.mean-f.surpriseBase.mad)
	low := 0
	for _, v := range f.surpriseWindow {
		if v < lowThreshold {
			low++
		}
	}
	lowFrac := 0.0
	if len(f.surpriseWindow) > 0 {
		lowFrac = float64(low) / float64(len(f.surpriseWindow))
	}

	entropy := normalizedEntropy(charCounts)
	maxRun := math.Min(float64(f.maxRun), 40) / 40
	maxShingle := math.Min(float64(f.repetition.maxCount), 30) / 30
	ttr := 1.0
	if len(words) > 0 {
		ttr = float64(len(unique)) / float64(len(words))
	}
	foreignFrac := 0.0
	if letters > 0 {
		foreignFrac = float64(foreign) / float64(letters)
	}

	return featureSnapshot{
		DigitFrac: float64(counts["digit"]) / float64(n), ForeignFrac: foreignFrac,
		RepeatRate: f.repetition.rate(), ZlibRatio: f.zlibRatio, TTR: ttr,
		ScriptSwitchRate:  float64(f.switches) / float64(maxInt(1, f.total)) * 100,
		StructuralDensity: float64(structHits) / math.Max(1, float64(n)/100),
		SymbolFrac:        float64(counts["symbol"]) / float64(n), MaxCharRun: maxRun,
		SpaceFrac: float64(counts["space"]) / float64(n),
		SurpriseZ: f.surpriseBase.z(curSurprise), SurpriseLowFrac: lowFrac,
		SimHashDrift: f.simhash.drift(), Entropy: entropy, MaxShingleCount: maxShingle,
	}
}

var structuralPatterns = []*regexp.Regexp{
	regexp.MustCompile(`#{3,}`), regexp.MustCompile("```"),
	regexp.MustCompile(`(?i)\bpip\s+install\b`), regexp.MustCompile(`(?i)\bnpm\s+install\b`),
	regexp.MustCompile(`#REF!`), regexp.MustCompile(`#DIV/0!`), regexp.MustCompile(`#VALUE!`),
	regexp.MustCompile(`https?://\S+`), regexp.MustCompile(`[A-Za-z]:\\\\`),
	regexp.MustCompile(`(?i)\.(py|env|json)\b`), regexp.MustCompile(`\d{4}年\d{1,2}月`),
	regexp.MustCompile(`={4,}`), regexp.MustCompile(`─{4,}`), regexp.MustCompile(`_{6,}`),
}

func classifyRune(r rune) (class, script string) {
	if unicode.IsDigit(r) {
		return "digit", ""
	}
	if unicode.IsSpace(r) {
		return "space", ""
	}
	if r < 128 && !unicode.IsLetter(r) && !unicode.IsDigit(r) {
		return "symbol", ""
	}
	switch {
	case inRanges(r, [][2]rune{{0x3400, 0x4DBF}, {0x4E00, 0x9FFF}, {0xF900, 0xFAFF}}):
		return "cjk", "cjk"
	case inRanges(r, [][2]rune{{0x3040, 0x30FF}}):
		return "kana", "kana"
	case inRanges(r, [][2]rune{{0xAC00, 0xD7AF}, {0x1100, 0x11FF}}):
		return "hangul", "hangul"
	case inRanges(r, [][2]rune{{0x0600, 0x06FF}, {0x0750, 0x077F}, {0xFB50, 0xFDFF}, {0xFE70, 0xFEFF}}):
		return "arabic", "arabic"
	case inRanges(r, [][2]rune{{0x0900, 0x097F}}):
		return "devanagari", "devanagari"
	case inRanges(r, [][2]rune{{0x0E00, 0x0E7F}}):
		return "thai", "thai"
	case inRanges(r, [][2]rune{{0x0590, 0x05FF}}):
		return "hebrew", "hebrew"
	case inRanges(r, [][2]rune{{0x0370, 0x03FF}}):
		return "greek", "greek"
	case inRanges(r, [][2]rune{{0x0400, 0x04FF}}):
		return "cyrillic", "cyrillic"
	case inRanges(r, [][2]rune{{0x0041, 0x024F}}):
		return "latin", "latin"
	case unicode.IsLetter(r):
		return "other", "other"
	default:
		return "other", ""
	}
}

func inRanges(r rune, ranges [][2]rune) bool {
	for _, pair := range ranges {
		if r >= pair[0] && r <= pair[1] {
			return true
		}
	}
	return false
}

func runExcluded(r rune) bool {
	return strings.ContainsRune("-=─_*#~•.│ \t\n", r)
}

func wordsIn(s string) []string {
	var words []string
	var word []rune
	flush := func() {
		if len(word) >= 2 {
			words = append(words, strings.ToLower(string(word)))
		}
		word = word[:0]
	}
	for _, r := range s {
		if unicode.IsLetter(r) {
			word = append(word, r)
		} else {
			flush()
		}
	}
	flush()
	return words
}

func compressionRatio(s string) float64 {
	if len([]rune(s)) < 120 {
		return 1
	}
	raw := []byte(s)
	var buf bytes.Buffer
	zw, _ := zlib.NewWriterLevel(&buf, 6)
	_, _ = zw.Write(raw)
	_ = zw.Close()
	return float64(buf.Len()) / float64(maxInt(1, len(raw)))
}

func normalizedEntropy(counts map[rune]int) float64 {
	total := 0
	for _, n := range counts {
		total += n
	}
	if total < 2 || len(counts) < 2 {
		return 1
	}
	h := 0.0
	for _, n := range counts {
		p := float64(n) / float64(total)
		h -= p * math.Log2(p)
	}
	return h / math.Log2(float64(len(counts)))
}

type robustEWMA struct {
	alpha     float64
	mean, mad float64
	n         int
	frozen    bool
}

func (r *robustEWMA) update(x float64) {
	if r.frozen {
		return
	}
	r.n++
	if r.n == 1 {
		r.mean = x
		return
	}
	previous := r.mean
	r.mean += r.alpha * (x - r.mean)
	r.mad += r.alpha * (math.Abs(x-previous) - r.mad)
}

func (r *robustEWMA) freeze() { r.frozen = true }

func (r *robustEWMA) z(x float64) float64 {
	if r.n < 3 {
		return 0
	}
	return (x - r.mean) / (1.4826*r.mad + 1e-6)
}

type onlineCharNGram struct {
	order       int
	alpha       float64
	maxContexts int
	counts      []map[string]map[rune]int
	totals      []map[string]int
	alphabet    map[rune]bool
	history     []rune
	weights     []float64
}

func newOnlineCharNGram() *onlineCharNGram {
	counts := make([]map[string]map[rune]int, 4)
	totals := make([]map[string]int, 4)
	for i := range counts {
		counts[i] = map[string]map[rune]int{}
		totals[i] = map[string]int{}
	}
	return &onlineCharNGram{order: 3, alpha: 0.02, maxContexts: 60000,
		counts: counts, totals: totals, alphabet: map[rune]bool{},
		weights: []float64{0.55, 0.28, 0.12, 0.05}}
}

func (m *onlineCharNGram) feed(r rune) float64 {
	value := m.surprise(r)
	m.observe(r)
	return value
}

func (m *onlineCharNGram) surprise(r rune) float64 {
	vocab := math.Max(30, float64(len(m.alphabet)+1))
	probability := 0.0
	for order := m.order; order >= 0; order-- {
		context := suffixString(m.history, order)
		count := 0
		if bucket := m.counts[order][context]; bucket != nil {
			count = bucket[r]
		}
		total := m.totals[order][context]
		p := (float64(count) + m.alpha) / (float64(total) + m.alpha*vocab)
		probability += m.weights[m.order-order] * p
	}
	probability = math.Min(1, math.Max(1e-9, probability))
	return -math.Log2(probability)
}

func (m *onlineCharNGram) observe(r rune) {
	for order := 0; order <= m.order; order++ {
		context := suffixString(m.history, order)
		bucket := m.counts[order][context]
		if bucket == nil {
			if len(m.counts[order]) >= m.maxContexts {
				continue
			}
			bucket = map[rune]int{}
			m.counts[order][context] = bucket
		}
		bucket[r]++
		m.totals[order][context]++
	}
	m.alphabet[r] = true
	m.history = append(m.history, r)
	if len(m.history) > m.order {
		m.history = m.history[len(m.history)-m.order:]
	}
}

func suffixString(runes []rune, n int) string {
	if n <= 0 {
		return ""
	}
	if n > len(runes) {
		n = len(runes)
	}
	return string(runes[len(runes)-n:])
}

type countMinSketch struct {
	rows [4][2048]uint32
}

func (c *countMinSketch) add(value string) int {
	estimate := int(^uint(0) >> 1)
	for row := 0; row < len(c.rows); row++ {
		seed := uint32(0x9E3779B1) * uint32(row+1)
		column := stableHash(value, seed) % uint64(len(c.rows[row]))
		c.rows[row][column]++
		if n := int(c.rows[row][column]); n < estimate {
			estimate = n
		}
	}
	return estimate
}

type repetitionTracker struct {
	sketch          countMinSketch
	total, repeats  int
	maxCount, since int
}

func newRepetitionTracker() *repetitionTracker { return &repetitionTracker{} }

func (r *repetitionTracker) push(tail []rune) {
	r.since++
	if r.since < 4 || len(tail) < 12 {
		return
	}
	r.since = 0
	estimate := r.sketch.add(string(tail[len(tail)-12:]))
	r.total++
	if estimate >= 2 {
		r.repeats++
	}
	if estimate > r.maxCount {
		r.maxCount = estimate
	}
}

func (r *repetitionTracker) rate() float64 {
	if r.total == 0 {
		return 0
	}
	return float64(r.repeats) / float64(r.total)
}

type rollingSimHash struct {
	window   int
	tokens   []uint64
	bitSum   [64]int
	baseline *uint64
}

func newRollingSimHash(window int) *rollingSimHash { return &rollingSimHash{window: window} }

func (s *rollingSimHash) push(token string) {
	hash := stableHash(token, 0)
	if len(s.tokens) == s.window {
		old := s.tokens[0]
		s.tokens = s.tokens[1:]
		for bit := 0; bit < 64; bit++ {
			if old&(uint64(1)<<bit) != 0 {
				s.bitSum[bit]--
			} else {
				s.bitSum[bit]++
			}
		}
	}
	s.tokens = append(s.tokens, hash)
	for bit := 0; bit < 64; bit++ {
		if hash&(uint64(1)<<bit) != 0 {
			s.bitSum[bit]++
		} else {
			s.bitSum[bit]--
		}
	}
}

func (s *rollingSimHash) value() uint64 {
	var value uint64
	for bit := 0; bit < 64; bit++ {
		if s.bitSum[bit] > 0 {
			value |= uint64(1) << bit
		}
	}
	return value
}

func (s *rollingSimHash) setBaseline() {
	if len(s.tokens) < minInt(8, s.window) {
		return
	}
	value := s.value()
	s.baseline = &value
}

func (s *rollingSimHash) drift() float64 {
	if s.baseline == nil || len(s.tokens) < minInt(8, s.window) {
		return 0
	}
	return float64(bits.OnesCount64(s.value()^*s.baseline)) / 64
}

func stableHash(value string, seed uint32) uint64 {
	var key []byte
	if seed != 0 {
		key = make([]byte, 8)
		binary.LittleEndian.PutUint64(key, uint64(seed))
	}
	hash, _ := blake2b.New(8, key)
	_, _ = hash.Write([]byte(value))
	return binary.LittleEndian.Uint64(hash.Sum(nil))
}

func mean(values []float64) float64 {
	if len(values) == 0 {
		return 0
	}
	total := 0.0
	for _, value := range values {
		total += value
	}
	return total / float64(len(values))
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
