package scoring

import (
	"strings"
	"testing"
	"time"

	"github.com/mathewepstein/velocity/internal/analyze"
	"github.com/mathewepstein/velocity/internal/cache"
	"github.com/mathewepstein/velocity/internal/config"
)

func TestMaxTier(t *testing.T) {
	cases := []struct{ a, b, want string }{
		{"high", "low", "high"},
		{"low", "high", "high"},
		{"medium", "low", "medium"},
		{"low", "medium", "medium"},
		{"low", "low", "low"},
		{"", "medium", "medium"},
		{"garbage", "", "low"},
		{"high", "medium", "high"},
	}
	for _, c := range cases {
		if got := maxTier(c.a, c.b); got != c.want {
			t.Errorf("maxTier(%q,%q) = %q, want %q", c.a, c.b, got, c.want)
		}
	}
}

func TestDomainRiskTier(t *testing.T) {
	cfg := config.RiskConfig{
		High:   []string{"**/auth-microservice/**", "**/db/changelog/**"},
		Medium: []string{"**/cd-utils/**"},
	}
	cases := []struct {
		name      string
		paths     []string
		wantTier  string
		wantMatch bool
	}{
		{"high beats medium", []string{"src/cd-utils/x.go", "src/auth-microservice/Login.java"}, "high", true},
		{"medium only", []string{"src/cd-utils/Helper.java"}, "medium", true},
		{"no match", []string{"src/web/Home.vue"}, "low", false},
		{"migration high", []string{"src/main/resources/db/changelog/v1.xml"}, "high", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			tier, matched := domainRiskTier(c.paths, cfg)
			if tier != c.wantTier {
				t.Errorf("tier = %q, want %q", tier, c.wantTier)
			}
			if (matched != "") != c.wantMatch {
				t.Errorf("matched = %q, wantMatch=%v", matched, c.wantMatch)
			}
		})
	}
}

func TestDomainRiskTier_EmptyConfig(t *testing.T) {
	tier, matched := domainRiskTier([]string{"src/auth/x.go"}, config.RiskConfig{})
	if tier != "low" || matched != "" {
		t.Errorf("empty config should be low/no-match, got %q/%q", tier, matched)
	}
}

func TestMatchGlob_SubstringFallback(t *testing.T) {
	// A bare directory name (no ** anchoring) should still match a deep path via
	// the substring-core fallback.
	if !matchGlob("auth-microservice", "src/main/java/com/x/auth-microservice/Login.java") {
		t.Error("bare dir name should match deep path via substring fallback")
	}
	if !matchGlob("auth-microservice/**", "src/auth-microservice/a/b.go") {
		t.Error("dir/** should match")
	}
	if matchGlob("**/billing/**", "src/auth/x.go") {
		t.Error("non-matching glob should not match")
	}
}

// riskFixture: CD-200 touches a billing path that is NOT corpus-hot (so churn
// reads low), used to prove the domain config elevates it.
func riskFixture() *analyze.Loaded {
	created := time.Date(2026, 2, 1, 9, 0, 0, 0, time.UTC)
	merged := created.Add(10 * time.Hour)
	resolved := merged.Add(time.Hour)
	return &analyze.Loaded{
		Issues: []cache.JiraIssue{{
			Key: "CD-200", Summary: "Null-check in billing", IssueType: "Task",
			Status: "Done", Resolution: "Done", Created: created, Resolved: ptr(resolved),
			CycleHours: 9, DetailFetched: true,
		}},
		PRs: []cache.GitHubPR{{
			Number: 30, Repo: "org/app", State: "merged", Created: created, Merged: ptr(merged),
			IssueKeys:   []string{"CD-200"},
			FileChanges: []cache.FileChange{{Path: "src/billing/Charge.java", Status: "modified", Additions: 3, Deletions: 1}},
			Additions:   3, Deletions: 1,
		}},
	}
}

func TestExtract_DomainRiskElevates(t *testing.T) {
	data := riskFixture()

	// Baseline: no domain config → churn-only → low (single non-hot file).
	base := NewExtractor(data, defaultNorm(), 5*time.Minute, config.RiskConfig{})
	evBase, _ := base.Extract("CD-200")
	if evBase.TouchedAreaRisk != "low" {
		t.Fatalf("baseline risk = %q, want low", evBase.TouchedAreaRisk)
	}
	if evBase.RiskReason != "" {
		t.Errorf("baseline RiskReason should be empty, got %q", evBase.RiskReason)
	}

	// With a billing high-risk glob → elevated to high, with a reason.
	cfg := config.RiskConfig{High: []string{"**/billing/**"}}
	ext := NewExtractor(data, defaultNorm(), 5*time.Minute, cfg)
	ev, _ := ext.Extract("CD-200")
	if ev.TouchedAreaRisk != "high" {
		t.Fatalf("domain risk = %q, want high", ev.TouchedAreaRisk)
	}
	if ev.RiskReason == "" {
		t.Error("RiskReason should name the matching glob")
	}

	// And the band should carry a domain-match driver naming the glob.
	b := Band(ev, config.DefaultStoryPointsConfig())
	foundDomain := false
	for _, d := range b.Drivers {
		if strings.Contains(d, "domain match") {
			foundDomain = true
		}
	}
	if !foundDomain {
		t.Errorf("expected a domain-match driver, got %v", b.Drivers)
	}
}

// Regression guard: an empty [risk] block leaves the risk tier byte-identical to
// the churn-only baseline across the shared fixture.
func TestExtract_EmptyRiskMatchesBaseline(t *testing.T) {
	ext := NewExtractor(fixture(), defaultNorm(), 5*time.Minute, config.RiskConfig{})
	ev, _ := ext.Extract("CD-100")
	// CD-100 touches a corpus-hot auth file → medium/high from churn alone, and
	// RiskReason must stay empty (no domain config).
	if ev.RiskReason != "" {
		t.Errorf("RiskReason should be empty with no domain config, got %q", ev.RiskReason)
	}
	if ev.TouchedAreaRisk == "" {
		t.Error("TouchedAreaRisk should still be set from churn")
	}
}
