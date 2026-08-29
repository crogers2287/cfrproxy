package proxy

import (
	"strings"

	"github.com/crogers2287/cfrproxy/internal/integrity"
	"github.com/crogers2287/cfrproxy/internal/store"
)

// outputObservation owns one response's observation state. It never controls
// delivery: Feed only records features, and Finish only annotates the trace.
// Keeping that boundary explicit prevents observe mode from accidentally
// becoming a response gate before thresholds have been calibrated locally.
type outputObservation struct {
	observer *integrity.Observer
	done     bool
}

func (p *Proxy) newOutputObservation(prov store.Provider, model string, ep *store.Endpoint, tr *store.Trace) *outputObservation {
	mode, profile, models := prov.IntegrityMode, prov.IntegrityProfile, prov.IntegrityModels
	if ep != nil {
		switch strings.ToLower(strings.TrimSpace(ep.IntegrityMode)) {
		case "off":
			return nil
		case "observe":
			mode, models = "observe", ep.IntegrityModels
			if strings.TrimSpace(ep.IntegrityProfile) != "" {
				profile = ep.IntegrityProfile
			}
		}
	}
	if !strings.EqualFold(strings.TrimSpace(mode), "observe") || !integrityModelSelected(models, prov.Name, model) {
		return nil
	}
	profile = integrity.NormalizeProfile(profile)
	tr.GuardMode, tr.GuardProfile = "observe", profile
	return &outputObservation{observer: integrity.NewObserver(profile)}
}

func (o *outputObservation) Feed(text string) {
	if o != nil && !o.done && text != "" {
		o.observer.Feed(text)
	}
}

func (o *outputObservation) Finish(tr *store.Trace) {
	if o == nil || o.done {
		return
	}
	o.done = true
	report := o.observer.Finish()
	tr.GuardState = report.State
	tr.GuardScore = report.Score
	tr.GuardMaxScore = report.MaxScore
	tr.GuardReason = strings.Join(report.Reasons, "; ")
	tr.GuardOnset = report.OnsetChar
	tr.GuardChars = report.TotalChars
	tr.GuardCheckpoints = report.Checkpoints
	tr.GuardData = report.DataJSON()
	tr.GuardExcerpt = report.Excerpt
}

// integrityModelSelected accepts the same simple comma-separated '*' globs as
// provider model filters. A pattern may match either the bare model or its
// provider-qualified name. Exclusions always win; with no positive patterns,
// the list means "all except exclusions".
func integrityModelSelected(raw, provider, model string) bool {
	patterns := splitList(raw)
	if len(patterns) == 0 {
		return true
	}
	qualified := provider + "/" + model
	hasPositive, positiveMatch := false, false
	for _, pattern := range patterns {
		exclude := strings.HasPrefix(pattern, "!")
		if exclude {
			pattern = strings.TrimSpace(strings.TrimPrefix(pattern, "!"))
		} else {
			hasPositive = true
		}
		if pattern == "" {
			continue
		}
		matches := matchGlob(pattern, model) || matchGlob(pattern, qualified)
		if exclude && matches {
			return false
		}
		if !exclude && matches {
			positiveMatch = true
		}
	}
	return !hasPositive || positiveMatch
}
