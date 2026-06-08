// Package scoring builds the story-points engine on top of velocity's cached
// GitHub + Jira corpus (storypoints-engine-plan.md). It is structured as
// stages with the TicketEvidence bundle as the stable contract/seam: extraction
// (this file) assembles evidence from the cache with no re-fetch; the band
// stage turns evidence into a deterministic Fibonacci band; write-back posts an
// approved score to Jira. Any scorer — velocity's, the org's, a teammate's —
// consumes the same TicketEvidence.
package scoring

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mathewepstein/velocity/internal/analyze"
	"github.com/mathewepstein/velocity/internal/cache"
	"github.com/mathewepstein/velocity/internal/config"
)

// EvidenceHash is a stable fingerprint of the bundle, used to make the generator
// idempotent: if the evidence hasn't changed since the last run, the band is
// not recomputed/rewritten. It hashes the canonical JSON encoding, so any
// material change to the cached inputs (a new PR, more comments, a status
// transition) flips it.
func EvidenceHash(ev *TicketEvidence) string {
	b, err := json.Marshal(ev)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8]) // 16 hex chars is plenty to detect change
}

// TicketEvidence is the frozen evidence bundle for one Jira ticket: everything a
// scorer needs, assembled from cache. It is the contract between extraction and
// any scorer, and serializes to JSON as velocity's data-provider surface.
//
// Field provenance: Jira* fields come straight from the cached JiraIssue
// (including the detail-hydration derivations CycleHours/StatusFlips/etc.); PR*
// and review aggregates are matched via the PR's pre-computed IssueKeys; Derived
// fields are computed here over the matched PRs and the corpus-wide file-touch
// frequency index.
type TicketEvidence struct {
	// --- Jira (from the cached JiraIssue) ---
	Key                 string     `json:"key"`
	Summary             string     `json:"summary"`
	Description         string     `json:"description,omitempty"`
	IssueType           string     `json:"issue_type,omitempty"`
	Status              string     `json:"status,omitempty"`
	Resolution          string     `json:"resolution,omitempty"`
	Created             time.Time  `json:"created"`
	Resolved            *time.Time `json:"resolved,omitempty"`
	EpicKey             string     `json:"epic_key,omitempty"`
	Labels              []string   `json:"labels,omitempty"`
	Components          []string   `json:"components,omitempty"`
	ExistingStoryPoints float64    `json:"existing_story_points,omitempty"`

	// --- Thinking / process signals (from changelog + comments, derived at
	// detail-hydration time and cached on the issue). ---
	CycleHours      float64    `json:"cycle_hours,omitempty"`       // In Progress → Done (raw, includes waits)
	QueueHours      float64    `json:"queue_hours,omitempty"`       // time parked in QA-queue / wait statuses
	ActiveCycleHours float64   `json:"active_cycle_hours,omitempty"` // cycle minus queue — the band axis
	StatusFlips     int        `json:"status_flips,omitempty"`      // raw re-entry count (display only)
	ReworkCount     int        `json:"rework_count,omitempty"`      // backward bounces review/QA→dev — the band signal
	PreCodeComments int        `json:"pre_code_comments,omitempty"`
	TotalComments   int        `json:"total_comments,omitempty"`
	// Spike artifact-density signals, derived from description + comment bodies at
	// extraction time. ArtifactLinks counts distinct doc/planning URLs (Confluence,
	// MCP, implementation/discovery markdown); SubstantiveComments counts comments
	// bearing a code fence, a URL, or above a built-in length floor. Both feed the
	// spike scorer's artifact axis; they are inert on the standard band path.
	ArtifactLinks       int `json:"artifact_links,omitempty"`
	SubstantiveComments int `json:"substantive_comments,omitempty"`
	FirstInProgress *time.Time `json:"first_in_progress,omitempty"`
	DoneAt          *time.Time `json:"done_at,omitempty"`
	DetailFetched   bool       `json:"detail_fetched"`

	// --- Matched pull requests. ---
	PRs []PREvidence `json:"prs,omitempty"`

	// --- Typing + review rollups across the matched PRs. ---
	RawLOC                  int      `json:"raw_loc"`              // additions+deletions, all files
	NetLOC                  int      `json:"net_loc"`              // additions+deletions excl. generated files
	FileCount               int      `json:"file_count"`           // distinct non-generated paths
	DirSpread               int      `json:"dir_spread"`           // distinct top-level dirs touched
	TestFilesTouched        int      `json:"test_files_touched"`   // distinct test-file paths
	Repos                   []string `json:"repos,omitempty"`      // distinct repos touched
	ReviewRounds            int      `json:"review_rounds"`        // CHANGES_REQUESTED reviews across PRs
	DistinctReviewers       int      `json:"distinct_reviewers"`   // union of reviewers across PRs
	InlineComments    int     `json:"inline_comments"` // sum of PR inline review comments
	DeepThreads       int     `json:"deep_threads"`    // sum of 3+-reply inline threads
	TimeToMergeHours  float64 `json:"time_to_merge_hours,omitempty"` // max across matched PRs

	// --- Touched-area risk (corpus-relative). ---
	TouchedAreaRisk string   `json:"touched_area_risk"`  // low | medium | high
	HotFiles        []string `json:"hot_files,omitempty"` // touched files in the top corpus-frequency tier
	// RiskReason names the configured domain-risk glob that drove (or tied for)
	// the tier, when the domain dimension is what elevated it. Empty when the
	// churn/hot-file signal drove the tier — the band's driver text then falls
	// back to the hot-file preview.
	RiskReason string `json:"risk_reason,omitempty"`
}

// PREvidence is the per-PR slice of the bundle for one matched pull request.
type PREvidence struct {
	Number            int        `json:"number"`
	Repo              string     `json:"repo"`
	Title             string     `json:"title"`
	Author            string     `json:"author,omitempty"`
	Additions         int        `json:"additions"`
	Deletions         int        `json:"deletions"`
	FileCount         int        `json:"file_count"`
	TestFiles         int        `json:"test_files"`
	Created           time.Time  `json:"created"`
	Merged            *time.Time `json:"merged,omitempty"`
	ReviewRounds      int        `json:"review_rounds"`      // CHANGES_REQUESTED reviews on this PR
	DistinctReviewers int        `json:"distinct_reviewers"`
	InlineComments    int        `json:"inline_comments"`
	DeepThreads       int        `json:"deep_threads"`
}

// Extractor assembles TicketEvidence from a loaded corpus. Build it once (it
// constructs reverse indices over the whole corpus) and Extract many tickets.
type Extractor struct {
	norm    config.NormalizeConfig
	riskCfg config.RiskConfig // config-driven domain risk (empty = churn-only baseline)

	issueByKey map[string]*cache.JiraIssue
	prsByKey   map[string][]*cache.GitHubPR // ticket key -> PRs referencing it
	reviewsByPR map[string][]cache.GitHubReview
	commitTimesByKey map[string][]time.Time // ticket key -> linked commit timestamps (for de-noised rework)
	reworkMinDwell   time.Duration  // rescue threshold for the rework de-noiser (StoryPoints.ReworkMinDwell)
	fileFreq   map[string]int // corpus-wide count of PRs touching each path
	hotCutoff  int            // file-frequency threshold for "hot"
}

// BuildExtractor loads the full corpus from store for profile and returns a
// ready Extractor. This is the per-request entry point for the server's scoring
// endpoints (the same per-request LoadAll the other /api/* handlers use). now
// resolves the current month for the load window.
func BuildExtractor(profile config.Profile, store cache.Store, now time.Time) (*Extractor, error) {
	data, err := analyze.LoadAll(profile, cache.CurrentMonth(now), store)
	if err != nil {
		return nil, err
	}
	return NewExtractor(data, profile.Scoring.Normalize, profile.StoryPoints.ReworkMinDwell(), profile.StoryPoints.Risk), nil
}

// NewExtractor builds the reverse indices over data. norm supplies the
// generated-file patterns used to compute net (non-generated) LOC, matching the
// code_impact normalizer. reworkMinDwell tunes the rework de-noiser (a
// commit-less backward bounce shorter than this is treated as toggle noise).
// riskCfg adds the config-driven domain-risk dimension; an empty RiskConfig
// leaves touched-area risk byte-identical to the churn-only baseline.
func NewExtractor(data *analyze.Loaded, norm config.NormalizeConfig, reworkMinDwell time.Duration, riskCfg config.RiskConfig) *Extractor {
	e := &Extractor{
		norm:             norm,
		riskCfg:          riskCfg,
		issueByKey:       make(map[string]*cache.JiraIssue, len(data.Issues)),
		prsByKey:         make(map[string][]*cache.GitHubPR),
		reviewsByPR:      make(map[string][]cache.GitHubReview),
		commitTimesByKey: make(map[string][]time.Time),
		reworkMinDwell:   reworkMinDwell,
		fileFreq:         make(map[string]int),
	}
	for i := range data.Issues {
		iss := &data.Issues[i]
		e.issueByKey[strings.ToUpper(iss.Key)] = iss
	}
	// Commit timestamps per issue key — the primary signal for de-noising rework
	// (a backward bounce is real if code landed after it).
	for i := range data.Commits {
		c := &data.Commits[i]
		for _, k := range c.IssueKeys {
			ku := strings.ToUpper(k)
			e.commitTimesByKey[ku] = append(e.commitTimesByKey[ku], c.Committed)
		}
	}
	for i := range data.PRs {
		pr := &data.PRs[i]
		for _, k := range pr.IssueKeys {
			ku := strings.ToUpper(k)
			e.prsByKey[ku] = append(e.prsByKey[ku], pr)
		}
		// Corpus-wide file-touch frequency: distinct files per PR so a single
		// huge PR doesn't inflate a path's hotness.
		for f := range distinctPaths(pr) {
			e.fileFreq[f]++
		}
	}
	for _, rv := range data.Reviews {
		key := prRefKey(rv.Repo, rv.PRNumber)
		e.reviewsByPR[key] = append(e.reviewsByPR[key], rv)
	}
	e.hotCutoff = hotFileCutoff(e.fileFreq)
	return e
}

// Keys returns every cached Jira ticket key, deduped and sorted. The generator
// sweeps these.
func (e *Extractor) Keys() []string {
	out := make([]string, 0, len(e.issueByKey))
	for _, iss := range e.issueByKey {
		out = append(out, iss.Key)
	}
	sort.Strings(out)
	return out
}

// Extract assembles the evidence bundle for ticketKey. It returns (nil, false)
// if the ticket is not in the cache. A ticket with no matched PRs is still
// returned (Jira-only evidence) — that is itself a signal.
func (e *Extractor) Extract(ticketKey string) (*TicketEvidence, bool) {
	ku := strings.ToUpper(strings.TrimSpace(ticketKey))
	iss, ok := e.issueByKey[ku]
	if !ok {
		return nil, false
	}

	ev := &TicketEvidence{
		Key:                 iss.Key,
		Summary:             iss.Summary,
		Description:         iss.Description,
		IssueType:           iss.IssueType,
		Status:              iss.Status,
		Resolution:          iss.Resolution,
		Created:             iss.Created,
		Resolved:            iss.Resolved,
		EpicKey:             iss.EpicKey,
		Labels:              iss.Labels,
		Components:          iss.Components,
		ExistingStoryPoints: iss.StoryPoints,
		CycleHours:          iss.CycleHours,
		StatusFlips:         iss.StatusFlips,
		ReworkCount:         analyze.ReworkCountWithCommits(*iss, e.commitTimesByKey[ku], e.reworkMinDwell),
		// Active cycle = In-Progress→Done minus QA-queue/wait time, so a long
		// Ready-QA queue (dead time at CD, ~66h median) doesn't inflate the band.
		// Only meaningful when the changelog gave us a real cycle; left 0 when
		// CycleHours is absent (band then falls back to Created→Resolved).
		QueueHours:       queueHoursOf(iss),
		ActiveCycleHours: activeCycleHoursOf(iss),
		PreCodeComments:     iss.PreCodeComments,
		TotalComments:       len(iss.Comments),
		FirstInProgress:     iss.FirstInProgress,
		DoneAt:              iss.DoneAt,
		DetailFetched:       iss.DetailFetched,
	}
	ev.ArtifactLinks, ev.SubstantiveComments = spikeArtifactSignals(iss)

	prs := e.prsByKey[ku]
	reviewers := map[string]struct{}{}
	repos := map[string]struct{}{}
	netFiles := map[string]struct{}{}
	dirs := map[string]struct{}{}
	hot := map[string]struct{}{}
	testFiles := map[string]struct{}{}

	for _, pr := range prs {
		pe := PREvidence{
			Number:    pr.Number,
			Repo:      pr.Repo,
			Title:     pr.Title,
			Author:    pr.Author,
			Additions: pr.Additions,
			Deletions: pr.Deletions,
			Created:   pr.Created,
			Merged:    pr.Merged,
		}
		ev.RawLOC += pr.Additions + pr.Deletions

		paths := distinctPaths(pr)
		prTests := 0
		for f := range paths {
			repos[pr.Repo] = struct{}{}
			if analyze.IsGeneratedPath(f, e.norm) {
				continue
			}
			netFiles[f] = struct{}{}
			if d := topDir(f); d != "" {
				dirs[d] = struct{}{}
			}
			if isTestFile(f) {
				testFiles[f] = struct{}{}
				prTests++
			}
			// A file is "hot/risky" only if it's both high-churn AND real code.
			// Test files and config/i18n/data resources are high-churn by nature
			// (every feature edits them) but touching them isn't risk — counting
			// them inflated the risk signal (e.g. messages.properties is the most-
			// touched path in the whole corpus). Exclude them from the hot set; the
			// frequency index + cutoff are left intact so genuine hot code is
			// unaffected.
			if e.hotCutoff > 0 && e.fileFreq[f] >= e.hotCutoff && !isRiskExcludedFile(f) {
				hot[f] = struct{}{}
			}
		}
		pe.FileCount = len(paths)
		pe.TestFiles = prTests

		// Reviews: CHANGES_REQUESTED count = rework rounds; union reviewers.
		rvs := e.reviewsByPR[prRefKey(pr.Repo, pr.Number)]
		prReviewers := map[string]struct{}{}
		for _, rv := range rvs {
			if rv.Reviewer != "" {
				prReviewers[rv.Reviewer] = struct{}{}
				reviewers[rv.Reviewer] = struct{}{}
			}
			if rv.State == "CHANGES_REQUESTED" {
				pe.ReviewRounds++
				ev.ReviewRounds++
			}
		}
		pe.DistinctReviewers = len(prReviewers)
		pe.InlineComments = pr.InlineComments
		pe.DeepThreads = pr.DeepThreads
		ev.InlineComments += pr.InlineComments
		ev.DeepThreads += pr.DeepThreads

		if pr.Merged != nil {
			if ttm := pr.Merged.Sub(pr.Created).Hours(); ttm > ev.TimeToMergeHours {
				ev.TimeToMergeHours = ttm
			}
		}
		ev.PRs = append(ev.PRs, pe)
	}

	// Net LOC: re-sum per-file additions+deletions over non-generated files when
	// FileChanges are available; otherwise fall back to RawLOC (path list only).
	ev.NetLOC = netLOC(prs, e.norm)
	ev.FileCount = len(netFiles)
	ev.DirSpread = len(dirs)
	ev.TestFilesTouched = len(testFiles)
	ev.DistinctReviewers = len(reviewers)
	ev.Repos = sortedKeys(repos)
	ev.HotFiles = sortedKeys(hot)

	// Touched-area risk = max(churn-derived tier, config domain tier). The churn
	// signal (hot files) is the zero-config baseline; the domain dimension scans
	// every non-generated touched path (including config/migration resources that
	// the churn signal deliberately excludes) against the configured globs and
	// only ever elevates. RiskReason records the matching glob when domain drove
	// (or tied for) the tier, so the band can explain location-based risk.
	churnTier := riskBand(len(hot), len(netFiles))
	domainTier, matched := domainRiskTier(sortedKeys(netFiles), e.riskCfg)
	ev.TouchedAreaRisk = maxTier(churnTier, domainTier)
	if domainTier != "low" && riskTierRank(domainTier) >= riskTierRank(churnTier) {
		ev.RiskReason = matched
	}
	return ev, true
}

// queueHoursOf returns the issue's QA-queue/wait time (0 when no cycle is known).
func queueHoursOf(iss *cache.JiraIssue) float64 {
	if iss.CycleHours <= 0 {
		return 0
	}
	return analyze.QueueHours(*iss)
}

// activeCycleHoursOf returns the time the issue spent in active dev/review
// statuses (In Progress / Work in progress / Code Review), summed across
// re-entries. This is dormancy-free by construction: backlog deprioritization
// (Open/Selected/Blocked), QA queue, and pre-dev "Reviewed" limbo are excluded,
// so a ticket that sat for months no longer inflates the band's cycle axis.
// Zero when the changelog shows no active dev time (band falls back to
// CycleHours, then Created→Resolved).
func activeCycleHoursOf(iss *cache.JiraIssue) float64 {
	return analyze.ActiveDevHours(*iss)
}

// netLOC sums additions+deletions over non-generated files using per-file
// FileChanges where present (the precise signal), falling back to the PR-level
// additions+deletions when a PR has no FileChanges cached.
func netLOC(prs []*cache.GitHubPR, norm config.NormalizeConfig) int {
	total := 0
	for _, pr := range prs {
		if len(pr.FileChanges) == 0 {
			total += pr.Additions + pr.Deletions
			continue
		}
		for _, fc := range pr.FileChanges {
			if analyze.IsGeneratedPath(fc.Path, norm) {
				continue
			}
			total += fc.Additions + fc.Deletions
		}
	}
	return total
}

// distinctPaths returns the set of file paths a PR touched, preferring
// FileChanges (status-bearing) and falling back to the path-only Files list.
func distinctPaths(pr *cache.GitHubPR) map[string]struct{} {
	out := map[string]struct{}{}
	if len(pr.FileChanges) > 0 {
		for _, fc := range pr.FileChanges {
			out[fc.Path] = struct{}{}
		}
		return out
	}
	for _, f := range pr.Files {
		out[f] = struct{}{}
	}
	return out
}

// NOTE: "commits after first review" from the rubric is intentionally omitted.
// The cache links commits to issue keys, not to PR numbers, so it cannot be
// sourced without a commit->PR join we do not have. ReviewRounds (count of
// CHANGES_REQUESTED reviews) carries the rework signal the rubric actually uses.

// prRefKey is the index key joining reviews to their PR.
func prRefKey(repo string, number int) string {
	return repo + "#" + strconv.Itoa(number)
}

// topDir returns the first path segment of p, or "" for a root-level file.
func topDir(p string) string {
	p = strings.TrimPrefix(p, "./")
	if i := strings.IndexByte(p, '/'); i > 0 {
		return p[:i]
	}
	return ""
}

// isTestFile flags common test-file conventions across the languages in the
// corpus (Go, JS/TS, Vue, Java/JSP).
func isTestFile(p string) bool {
	pl := strings.ToLower(p)
	base := path.Base(pl)
	if strings.HasSuffix(base, "_test.go") {
		return true
	}
	if strings.Contains(base, ".test.") || strings.Contains(base, ".spec.") {
		return true
	}
	for _, seg := range []string{"/test/", "/tests/", "/__tests__/", "/spec/", "/e2e/"} {
		if strings.Contains(pl, seg) {
			return true
		}
	}
	return strings.HasPrefix(pl, "test/") || strings.HasPrefix(pl, "tests/")
}

// isRiskExcludedFile reports whether a high-churn file should be kept OUT of the
// touched-area-risk signal. Test files and non-code resources (config, i18n,
// data, lockfiles, build descriptors) churn constantly without carrying risk, so
// their churn is boilerplate, not danger.
func isRiskExcludedFile(p string) bool {
	return isTestFile(p) || isResourceFile(p)
}

// riskExcludedExt is the set of non-code file extensions excluded from the
// touched-area-risk signal. These are config/i18n/data/build files: frequently
// edited, but editing them is rarely the risky part of a change. Code
// extensions (.java/.go/.ts/.vue/.kt/.swift/.py/.sql/.html/…) are intentionally
// absent so genuine hot code still registers as risk.
var riskExcludedExt = map[string]bool{
	".properties": true, ".json": true, ".yaml": true, ".yml": true, ".xml": true,
	".csv": true, ".toml": true, ".ini": true, ".lock": true, ".cfg": true,
	".conf": true, ".gradle": true, ".md": true, ".txt": true, ".map": true,
}

// isResourceFile reports whether p is a non-code config/i18n/data/build file.
func isResourceFile(p string) bool {
	return riskExcludedExt[strings.ToLower(path.Ext(p))]
}

// hotFileCutoff picks the PR-touch-frequency threshold above which a file counts
// as "hot" (high-churn / high-risk area). It is the 95th percentile of the
// per-file touch counts, floored at 5 so a sparse corpus doesn't flag everything.
func hotFileCutoff(freq map[string]int) int {
	if len(freq) == 0 {
		return 0
	}
	counts := make([]int, 0, len(freq))
	for _, c := range freq {
		counts = append(counts, c)
	}
	sort.Ints(counts)
	idx := int(0.95 * float64(len(counts)-1))
	cut := counts[idx]
	if cut < 5 {
		cut = 5
	}
	return cut
}

// riskBand maps the count of hot files touched (and the overall file count) to a
// coarse low/medium/high risk label.
func riskBand(hotCount, fileCount int) string {
	switch {
	case hotCount >= 3:
		return "high"
	case hotCount >= 1:
		return "medium"
	default:
		return "low"
	}
}

func sortedKeys(m map[string]struct{}) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
