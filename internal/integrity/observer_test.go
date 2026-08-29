package integrity

import (
	"strings"
	"testing"
)

const cleanText = "The quarterly report showed steady growth across all regions. " +
	"Revenue increased year over year, driven mostly by enterprise subscriptions. " +
	"Operating expenses rose more slowly, so margins expanded for the third consecutive quarter. " +
	"Management expects this trend to continue into the next fiscal year, provided that input costs remain stable. "

func TestCleanTextStaysClean(t *testing.T) {
	observer := NewObserver(ProfileGeneral)
	observer.Feed(strings.Repeat(cleanText, 4))
	report := observer.Finish()
	if report.State != StateClean {
		t.Fatalf("clean text classified %s: score %.2f reasons %v", report.State, report.MaxScore, report.Reasons)
	}
	if report.Checkpoints < 2 || len(report.Samples) < 2 {
		t.Fatalf("expected checkpoint data, got %+v", report)
	}
}

func TestRepetitionCollapseIsObserved(t *testing.T) {
	observer := NewObserver(ProfileGeneral)
	observer.Feed(cleanText)
	observer.Feed(strings.Repeat("the same phrase keeps repeating in a tight loop ", 80))
	report := observer.Finish()
	if report.State != StateCorrupt {
		t.Fatalf("repetition classified %s: score %.2f reasons %v", report.State, report.MaxScore, report.Reasons)
	}
	if report.OnsetChar == 0 || report.Excerpt == "" {
		t.Fatalf("corrupt report missing review context: %+v", report)
	}
	if !hasClass(report.Classes, "repetition_collapse") {
		t.Fatalf("missing repetition class: %v", report.Classes)
	}
}

func TestForeignScriptDriftCanBeDisabled(t *testing.T) {
	drift := strings.Repeat("由国立大学金融科技研发团队主导开发的智能合约审计系统正式开放企业试用", 40)
	general := NewObserver(ProfileGeneral)
	general.Feed(cleanText + drift)
	if report := general.Finish(); report.State != StateCorrupt {
		t.Fatalf("general profile should flag script drift: %+v", report)
	}

	multilingual := NewObserver(ProfileMultilingual)
	multilingual.Feed(cleanText + drift)
	report := multilingual.Finish()
	for _, reason := range report.Reasons {
		if strings.Contains(reason, "foreign-script") {
			t.Fatalf("multilingual profile emitted foreign-script reason: %+v", report)
		}
	}
}

func TestCodeProfileDoesNotTreatCodeMarkersAsCorruption(t *testing.T) {
	code := strings.Repeat("## Install\n```bash\npip install package\n```\nSee https://example.com/config.json\n", 20)
	observer := NewObserver(ProfileCode)
	observer.Feed(code)
	report := observer.Finish()
	for _, reason := range report.Reasons {
		if strings.Contains(reason, "structural artifacts") || strings.Contains(reason, "numeric dump") {
			t.Fatalf("code-safe profile used prose-only rule: %+v", report)
		}
	}
}

func TestSamplesAreBounded(t *testing.T) {
	observer := NewObserver(ProfileGeneral)
	observer.Feed(strings.Repeat(cleanText, 150))
	report := observer.Finish()
	if len(report.Samples) != maxSamples {
		t.Fatalf("samples=%d want %d", len(report.Samples), maxSamples)
	}
	if report.Samples[0].Char != defaultHold {
		t.Fatalf("first checkpoint was not retained: %+v", report.Samples[0])
	}
	if report.Samples[len(report.Samples)-1].Char != report.TotalChars {
		t.Fatalf("last checkpoint was not retained: last=%d total=%d", report.Samples[len(report.Samples)-1].Char, report.TotalChars)
	}
}

func TestToolOnlyOutputIsSkippedInsteadOfCleanCalibrationData(t *testing.T) {
	report := NewObserver(ProfileGeneral).Finish()
	if report.State != StateSkipped || report.Checkpoints != 0 || len(report.Samples) != 0 {
		t.Fatalf("empty visible output should be skipped: %+v", report)
	}
}

func hasClass(classes []string, want string) bool {
	for _, class := range classes {
		if class == want {
			return true
		}
	}
	return false
}
