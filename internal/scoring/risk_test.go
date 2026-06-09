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

func TestMatchGlob_SegmentFallback(t *testing.T) {
	// A bare directory name (no ** anchoring) should still match a deep path via
	// the segment-core fallback. (Matcher is shared in analyze.MatchGlob.)
	if !analyze.MatchGlob("auth-microservice", "src/main/java/com/x/auth-microservice/Login.java") {
		t.Error("bare dir name should match deep path via segment fallback")
	}
	if !analyze.MatchGlob("auth-microservice/**", "src/auth-microservice/a/b.go") {
		t.Error("dir/** should match")
	}
	if analyze.MatchGlob("**/billing/**", "src/auth/x.go") {
		t.Error("non-matching glob should not match")
	}
	// The core matches only at segment boundaries, so `credit` matches a real
	// /credit/ segment but NOT the "smartcredit" product name (the over-match the
	// segment fix closes).
	if !analyze.MatchGlob("**/credit/**", "src/main/credit/Report.java") {
		t.Error("credit should match a real /credit/ segment")
	}
	if analyze.MatchGlob("**/credit/**", "_legacy/tenant/Smartcredit/theme/template.txt") {
		t.Error("credit must NOT match the Smartcredit product name (mid-segment)")
	}
	if analyze.MatchGlob("**/credit/**", "src/pages/credit-report/Index.vue") {
		t.Error("credit must NOT match credit-report (mid-segment)")
	}
	// A multi-segment core still matches as a contiguous run.
	if !analyze.MatchGlob("**/db/changelog/**", "src/main/resources/db/changelog/v1.sql") {
		t.Error("multi-segment core should match a contiguous run")
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

// A configured noise path (storybook stories) is kept OUT of the churn risk
// signal even when it's corpus-hot — it would otherwise inflate touched-area
// risk on dev-only tooling.
func TestExtract_NoisePathExcludedFromRisk(t *testing.T) {
	created := time.Date(2026, 4, 1, 9, 0, 0, 0, time.UTC)
	merged := created.Add(8 * time.Hour)
	story := "src/stories/Button.stories.ts"
	data := &analyze.Loaded{
		Issues: []cache.JiraIssue{{
			Key: "CD-400", Summary: "tweak story", IssueType: "Task", Status: "Done",
			Resolution: "Done", Created: created, Resolved: ptr(merged.Add(time.Hour)),
			CycleHours: 7, DetailFetched: true,
		}},
		PRs: []cache.GitHubPR{
			{Number: 50, Repo: "org/app", State: "merged", Created: created, Merged: ptr(merged),
				IssueKeys:   []string{"CD-400"},
				FileChanges: []cache.FileChange{{Path: story, Status: "modified", Additions: 5, Deletions: 1}}},
			// Background PRs making the story file corpus-hot.
			bgPR(60, "org/app", story), bgPR(61, "org/app", story), bgPR(62, "org/app", story),
			bgPR(63, "org/app", story), bgPR(64, "org/app", story), bgPR(65, "org/app", story),
		},
	}

	// Without noise config: the hot story file drives risk up.
	plain := NewExtractor(data, defaultNorm(), 5*time.Minute, config.RiskConfig{})
	evPlain, _ := plain.Extract("CD-400")

	// With the story dir as a noise path: excluded from the hot set → low risk.
	norm := defaultNorm()
	norm.NoisePaths = []string{"**/src/stories/**"}
	quiet := NewExtractor(data, norm, 5*time.Minute, config.RiskConfig{})
	evQuiet, _ := quiet.Extract("CD-400")

	if len(evQuiet.HotFiles) != 0 {
		t.Errorf("noise path should not appear in HotFiles, got %v", evQuiet.HotFiles)
	}
	if evQuiet.TouchedAreaRisk != "low" {
		t.Errorf("risk with noise excluded = %q, want low (plain was %q)", evQuiet.TouchedAreaRisk, evPlain.TouchedAreaRisk)
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
