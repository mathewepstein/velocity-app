package analyze

import (
	"testing"
	"time"

	"github.com/mathewepstein/velocity/internal/cache"
	"github.com/mathewepstein/velocity/internal/config"
)

// A disabled config yields a nil weighter, and the nil-safe accessor weights
// every PR at 1.0 — the guarantee that scoring is byte-identical when off.
func TestNewIntegrationWeighter_DisabledIsNil(t *testing.T) {
	w := newIntegrationWeighter(&Loaded{}, config.IntegrationConfig{Enabled: false, Factor: 0.25, Threshold: 0.5})
	if w != nil {
		t.Fatal("disabled config should produce a nil weighter")
	}
	if got := w.weightFor(cache.GitHubPR{}); got != 1.0 {
		t.Errorf("nil weighter must weight every PR at 1.0, got %v", got)
	}
}

// When enabled, a clear integration PR gets the down-weight factor and a clean
// feature PR gets 1.0.
func TestNewIntegrationWeighter_EnabledWeights(t *testing.T) {
	merged := func(s string) *time.Time { tm, _ := time.Parse("2006-01-02", s); return &tm }
	feature := cache.GitHubPR{
		Number: 1, Repo: "org/app", Author: "alice", Branch: "feature/x", BaseBranch: "development",
		Merged: merged("2026-01-01"), IssueKeys: []string{"CD-1"}, InlineComments: 2,
		Commits: []cache.PRCommit{{SHA: "a", Author: "alice", ParentCount: 1}},
	}
	promo := cache.GitHubPR{
		Number: 2, Repo: "org/app", Author: "bob", Branch: "development", BaseBranch: "master",
		Merged: merged("2026-02-01"),
		Commits: []cache.PRCommit{
			{SHA: "a", Author: "alice", ParentCount: 1},
			{SHA: "m", Author: "bob", ParentCount: 2},
		},
	}
	data := &Loaded{PRs: []cache.GitHubPR{feature, promo}}
	w := newIntegrationWeighter(data, config.IntegrationConfig{Enabled: true, Factor: 0.25, Threshold: 0.5})
	if w == nil {
		t.Fatal("enabled config should produce a non-nil weighter")
	}
	if got := w.weightFor(feature); got != 1.0 {
		t.Errorf("feature PR weight = %v, want 1.0", got)
	}
	if got := w.weightFor(promo); got != 0.25 {
		t.Errorf("integration PR weight = %v, want 0.25 (factor)", got)
	}
}

// The converter overlays config tunables onto the classifier defaults and
// leaves unset fields at their default.
func TestIntegrationClassifierConfig_Overlay(t *testing.T) {
	got := integrationClassifierConfig(config.IntegrationConfig{
		Factor:    0.4,
		Threshold: 0.6,
		Weights:   map[string]float64{"reship": 5.0},
	})
	if got.DownweightFactor != 0.4 {
		t.Errorf("factor = %v, want 0.4", got.DownweightFactor)
	}
	if got.Threshold != 0.6 {
		t.Errorf("threshold = %v, want 0.6", got.Threshold)
	}
	if got.WeightReship != 5.0 {
		t.Errorf("reship weight = %v, want 5.0 (overridden)", got.WeightReship)
	}
	if got.WeightAuthorDiversity != 2.0 {
		t.Errorf("author_diversity weight = %v, want 2.0 (default, not overridden)", got.WeightAuthorDiversity)
	}
}
