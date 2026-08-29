// The scoring and detector-fusion logic in this file is a modified native Go
// adaptation of SIMURG (Copyright 2026 HAL-X AI / Farid Aghayev), Apache-2.0.
// See THIRD_PARTY_NOTICES.md and licenses/SIMURG-APACHE-2.0.txt.
package integrity

import (
	"encoding/json"
	"math"
	"sort"
	"strings"
)

const (
	StateClean   = "clean"
	StateSuspect = "suspect"
	StateCorrupt = "corrupt"
	StateSkipped = "skipped"

	ProfileGeneral      = "general"
	ProfileCode         = "code"
	ProfileMultilingual = "multilingual"

	defaultHold       = 350
	defaultCheckEvery = 400
	corruptThreshold  = 0.70
	suspectThreshold  = 0.50
	maxSamples        = 64
	maxExcerptRunes   = 2000
)

type Sample struct {
	Char     int                `json:"char"`
	State    string             `json:"state"`
	Score    float64            `json:"score"`
	Reasons  []string           `json:"reasons,omitempty"`
	Classes  []string           `json:"classes,omitempty"`
	Features map[string]float64 `json:"features"`
}

type Report struct {
	Profile     string   `json:"profile"`
	State       string   `json:"state"`
	Score       float64  `json:"score"`
	MaxScore    float64  `json:"max_score"`
	Reasons     []string `json:"reasons,omitempty"`
	Classes     []string `json:"classes,omitempty"`
	OnsetChar   int      `json:"onset_char,omitempty"`
	TotalChars  int      `json:"total_chars"`
	Checkpoints int      `json:"checkpoints"`
	Excerpt     string   `json:"excerpt,omitempty"`
	Samples     []Sample `json:"samples,omitempty"`
}

func (r Report) SamplesJSON() string {
	b, _ := json.Marshal(r.Samples)
	return string(b)
}

// DataJSON is the bounded, versioned calibration payload stored with a trace.
// Versioning lets a later offline calibration job reject incompatible feature
// sets instead of silently mixing measurements from different observers.
func (r Report) DataJSON() string {
	b, _ := json.Marshal(struct {
		Version         int      `json:"version"`
		FirstCheckChars int      `json:"first_check_chars"`
		CheckEveryChars int      `json:"check_every_chars"`
		Classes         []string `json:"classes,omitempty"`
		Samples         []Sample `json:"samples,omitempty"`
	}{Version: 1, FirstCheckChars: defaultHold, CheckEveryChars: defaultCheckEvery, Classes: r.Classes, Samples: r.Samples})
	return string(b)
}

type Observer struct {
	profile         string
	features        *streamFeatures
	nextCheck       int
	baselineFrozen  bool
	hotStreak       int
	pageHinkley     pageHinkley
	checkpointChars []int
	lastChecked     int
	report          Report
	recent          []rune
	capturing       bool
	excerpt         []rune
}

func NewObserver(profile string) *Observer {
	profile = NormalizeProfile(profile)
	return &Observer{
		profile: profile, features: newStreamFeatures(), nextCheck: defaultHold,
		report:      Report{Profile: profile, State: StateClean},
		pageHinkley: pageHinkley{delta: 0.02, lambda: 0.35, burnIn: 3},
	}
}

func NormalizeProfile(profile string) string {
	switch strings.ToLower(strings.TrimSpace(profile)) {
	case ProfileCode:
		return ProfileCode
	case ProfileMultilingual:
		return ProfileMultilingual
	default:
		return ProfileGeneral
	}
}

func (o *Observer) Feed(text string) {
	for _, r := range text {
		o.features.feedRune(r)
		o.recent = append(o.recent, r)
		if len(o.recent) > 800 {
			o.recent = o.recent[len(o.recent)-800:]
		}
		if o.capturing && len(o.excerpt) < maxExcerptRunes {
			o.excerpt = append(o.excerpt, r)
		}
		if o.features.total >= o.nextCheck {
			o.checkpoint()
			if !o.baselineFrozen {
				o.features.freezeBaseline()
				o.baselineFrozen = true
			}
			o.nextCheck += defaultCheckEvery
		}
	}
}

func (o *Observer) Finish() Report {
	o.features.flushWord()
	if o.features.total == 0 {
		o.report.State = StateSkipped
		return o.Report()
	}
	if o.lastChecked != o.features.total {
		o.checkpoint()
	}
	o.report.TotalChars = o.features.total
	o.report.Checkpoints = len(o.checkpointChars)
	o.report.Excerpt = string(o.excerpt)
	return o.Report()
}

func (o *Observer) Report() Report {
	r := o.report
	r.Reasons = append([]string(nil), r.Reasons...)
	r.Classes = append([]string(nil), r.Classes...)
	r.Samples = append([]Sample(nil), r.Samples...)
	return r
}

func (o *Observer) checkpoint() {
	features := o.features.snapshot(o.profile)
	score, reasons, classes, hardRule := scoreFeatures(features, o.profile)
	o.lastChecked = o.features.total
	o.checkpointChars = append(o.checkpointChars, o.features.total)
	o.pageHinkley.update(score)
	if score >= corruptThreshold {
		o.hotStreak++
	} else {
		o.hotStreak = 0
	}

	state := StateClean
	if hardRule || o.hotStreak >= 2 || (score >= corruptThreshold && !o.baselineFrozen) {
		state = StateCorrupt
	} else if score >= suspectThreshold || o.hotStreak == 1 {
		state = StateSuspect
	}

	sample := Sample{Char: o.features.total, State: state, Score: score,
		Reasons: reasons, Classes: classes, Features: features.mapValue()}
	o.appendSample(sample)
	o.report.Score = score
	if score > o.report.MaxScore {
		o.report.MaxScore = score
		o.report.Reasons = append([]string(nil), reasons...)
		o.report.Classes = append([]string(nil), classes...)
	}
	if severity(state) > severity(o.report.State) {
		o.report.State = state
		if state == StateCorrupt {
			o.report.OnsetChar = o.onset()
		}
	}
	if state != StateClean && !o.capturing {
		o.capturing = true
		o.excerpt = append(o.excerpt, o.recent...)
		if len(o.excerpt) > maxExcerptRunes {
			o.excerpt = o.excerpt[len(o.excerpt)-maxExcerptRunes:]
		}
	}
	if o.report.State == "" {
		o.report.State = StateClean
	}
}

func (o *Observer) appendSample(sample Sample) {
	if len(o.report.Samples) < maxSamples {
		o.report.Samples = append(o.report.Samples, sample)
		return
	}
	// Keep the first 16 checkpoints and a rolling window of the latest 48.
	copy(o.report.Samples[16:63], o.report.Samples[17:64])
	o.report.Samples[63] = sample
}

func (o *Observer) onset() int {
	if o.pageHinkley.alarmedAt > 0 {
		index := o.pageHinkley.alarmedAt - 1
		if index >= 0 && index < len(o.checkpointChars) {
			return o.checkpointChars[index]
		}
	}
	if len(o.checkpointChars) > 0 {
		return o.checkpointChars[len(o.checkpointChars)-1]
	}
	return 0
}

func severity(state string) int {
	switch state {
	case StateCorrupt:
		return 2
	case StateSuspect:
		return 1
	default:
		return 0
	}
}

type detectorScore struct {
	name    string
	value   float64
	reasons []string
	classes []string
}

func scoreFeatures(s featureSnapshot, profile string) (float64, []string, []string, bool) {
	rule := scoreRules(s, profile)
	detectors := []detectorScore{rule, scoreNGram(s), scoreSketch(s), scoreSimHash(s), scoreEntropy(s)}
	best := 0.0
	strong := 0
	var reasons, classes []string
	for _, detector := range detectors {
		if detector.value > best {
			best = detector.value
		}
		if detector.value >= 0.5 {
			strong++
			reasons = append(reasons, detector.reasons...)
			classes = append(classes, detector.classes...)
		}
	}
	if strong >= 2 {
		best = math.Min(1, best+0.12)
	}
	return best, uniqueStrings(reasons), uniqueSorted(classes), rule.value >= 0.85
}

func scoreRules(s featureSnapshot, profile string) detectorScore {
	score := 0.0
	var reasons, classes []string
	// Code and tool-heavy outputs legitimately contain dense digits, symbols,
	// paths, URLs, package commands, and fenced blocks. Keep measuring those
	// features, but do not let them accuse in the code-safe profile.
	if profile != ProfileCode && s.DigitFrac > 0.28 {
		weight := math.Min(1, (s.DigitFrac-0.28)/0.2)
		score = math.Max(score, 0.7+0.3*weight)
		reasons = append(reasons, formatReason("numeric dump digit_frac", s.DigitFrac))
		classes = append(classes, "structural_breakdown")
	}
	if profile != ProfileMultilingual && s.ForeignFrac > 0.05 {
		weight := math.Min(1, (s.ForeignFrac-0.05)/0.25)
		score = math.Max(score, 0.7+0.3*weight)
		reasons = append(reasons, formatReason("foreign-script frac", s.ForeignFrac))
		classes = append(classes, "cross_lingual_drift")
	}
	if s.RepeatRate > 0.55 && s.ZlibRatio < 0.32 {
		score = math.Max(score, 0.85)
		reasons = append(reasons, formatPair("repetition loop rate", s.RepeatRate, "zlib", s.ZlibRatio))
		classes = append(classes, "repetition_collapse")
	} else if s.RepeatRate > 0.72 {
		score = math.Max(score, 0.75)
		reasons = append(reasons, formatReason("repetition rate", s.RepeatRate))
		classes = append(classes, "repetition_collapse")
	}
	if s.MaxCharRun >= 0.5 {
		score = math.Max(score, 0.7)
		reasons = append(reasons, "long same-char run")
		classes = append(classes, "structural_breakdown")
	}
	if profile != ProfileCode && s.StructuralDensity > 1.2 {
		score = math.Max(score, 0.75)
		reasons = append(reasons, formatReason("structural artifacts density", s.StructuralDensity))
		classes = append(classes, "structural_breakdown", "regurgitation")
	}
	if s.TTR < 0.22 && s.RepeatRate > 0.45 {
		score = math.Max(score, 0.7)
		reasons = append(reasons, formatReason("vocabulary collapse ttr", s.TTR))
		classes = append(classes, "repetition_collapse")
	}
	if profile != ProfileCode {
		weak := 0
		for _, hit := range []bool{s.DigitFrac > 0.10, profile != ProfileMultilingual && s.ForeignFrac > 0.02,
			s.RepeatRate > 0.5, s.StructuralDensity > 0.6, s.SymbolFrac > 0.18} {
			if hit {
				weak++
			}
		}
		if weak >= 3 && score < 0.6 {
			score = 0.6
			reasons = append(reasons, "multiple weak anomalies")
		}
	}
	return detectorScore{name: "rules", value: score, reasons: reasons, classes: uniqueSorted(classes)}
}

func scoreNGram(s featureSnapshot) detectorScore {
	drift := ramp(s.SurpriseZ, 3, 9)
	repetition := 0.0
	if s.RepeatRate > 0.4 {
		repetition = ramp(s.SurpriseLowFrac, 0.85, 0.97)
	}
	score := math.Max(drift, repetition)
	var reasons, classes []string
	if drift >= 0.5 {
		reasons = append(reasons, formatReason("surprise spike z", s.SurpriseZ))
		classes = append(classes, "cross_lingual_drift", "regurgitation")
	}
	if repetition >= 0.5 {
		reasons = append(reasons, formatReason("surprise collapse low_frac", s.SurpriseLowFrac))
		classes = append(classes, "repetition_collapse")
	}
	return detectorScore{name: "ngram", value: score, reasons: reasons, classes: uniqueSorted(classes)}
}

func scoreSketch(s featureSnapshot) detectorScore {
	score := ramp(s.RepeatRate, 0.55, 0.9)
	if s.RepeatRate > 0.5 {
		score = math.Min(1, score+0.3*ramp(s.MaxShingleCount, 0.3, 0.8))
	}
	if score < 0.5 {
		return detectorScore{name: "sketch", value: score}
	}
	return detectorScore{name: "sketch", value: score,
		reasons: []string{formatPair("repetition rate", s.RepeatRate, "maxcount", s.MaxShingleCount*30)},
		classes: []string{"repetition_collapse"}}
}

func scoreSimHash(s featureSnapshot) detectorScore {
	score := 0.60 * ramp(s.SimHashDrift, 0.42, 0.52)
	if score < 0.5 {
		return detectorScore{name: "simhash", value: score}
	}
	return detectorScore{name: "simhash", value: score,
		reasons: []string{formatReason("semantic drift hamming", s.SimHashDrift)},
		classes: []string{"semantic_discontinuity", "regurgitation"}}
}

func scoreEntropy(s featureSnapshot) detectorScore {
	if s.Entropy >= 0.45 || (s.DigitFrac <= 0.25 && s.RepeatRate <= 0.5) {
		return detectorScore{name: "entropy"}
	}
	score := ramp(0.45-s.Entropy, 0, 0.25)*0.9 + 0.1
	return detectorScore{name: "entropy", value: score,
		reasons: []string{formatReason("entropy collapse H", s.Entropy)},
		classes: []string{"structural_breakdown", "repetition_collapse"}}
}

func ramp(value, low, high float64) float64 {
	if value <= low {
		return 0
	}
	if value >= high {
		return 1
	}
	return (value - low) / (high - low)
}

func formatReason(label string, value float64) string {
	return label + "=" + formatFloat(value)
}

func formatPair(a string, av float64, b string, bv float64) string {
	return a + "=" + formatFloat(av) + " " + b + "=" + formatFloat(bv)
}

func formatFloat(value float64) string {
	b, _ := json.Marshal(math.Round(value*100) / 100)
	return string(b)
}

func uniqueStrings(values []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value != "" && !seen[value] {
			seen[value] = true
			out = append(out, value)
		}
	}
	return out
}

func uniqueSorted(values []string) []string {
	out := uniqueStrings(values)
	sort.Strings(out)
	return out
}

type pageHinkley struct {
	delta, lambda float64
	burnIn        int
	n             int
	mean, sum     float64
	minimum       float64
	alarmedAt     int
}

func (p *pageHinkley) update(value float64) bool {
	p.n++
	p.mean += (value - p.mean) / float64(p.n)
	p.sum += value - p.mean - p.delta
	p.minimum = math.Min(p.minimum, p.sum)
	if p.n <= p.burnIn || p.alarmedAt != 0 {
		return false
	}
	if p.sum-p.minimum > p.lambda {
		p.alarmedAt = p.n
		return true
	}
	return false
}
