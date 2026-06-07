package initflow

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"

	"github.com/AlecAivazis/survey/v2"
	"github.com/mathewepstein/velocity/internal/cache"
	"github.com/mathewepstein/velocity/internal/config"
	"github.com/mathewepstein/velocity/internal/secrets"
)

// Options controls the flow. Out is where progress messages are written.
// NoEnv disables auto-filling from environment variables (ATLASSIAN_EMAIL,
// ATLASSIAN_API_TOKEN, GH_TOKEN/GITHUB_TOKEN, GITHUB_ORG); every field gets
// prompted.
type Options struct {
	Out   io.Writer
	NoEnv bool
}

// Run is the full interactive `velocity init`:
//   1. Prompt + validate Atlassian creds.
//   2. Prompt + validate Jira project keys.
//   3. Discover Story Points + Epic Link custom field IDs.
//   4. Prompt + validate GitHub PAT.
//   5. Prompt + validate GitHub org membership.
//   6. Commit: write config.toml, store both tokens in the keychain.
//
// Any validation failure aborts before the commit step, so partial state
// never ends up on disk or in the keychain.
func Run(ctx context.Context, opts Options) error {
	if opts.Out == nil {
		return fmt.Errorf("initflow.Options.Out is required")
	}
	w := opts.Out
	env := detectEnv(opts.NoEnv)
	env.PrintBanner(w)

	// --- Jira ---
	fmt.Fprintln(w, "Atlassian setup")
	fmt.Fprintln(w, "---------------")

	var baseURL, email, jiraToken string
	if err := askText(w, "Atlassian URL (or just your subdomain, e.g. consumerdirect)", "", &baseURL, requireNonEmpty); err != nil {
		return err
	}
	normalized, err := NormalizeJiraURL(baseURL)
	if err != nil {
		return err
	}
	if normalized != baseURL {
		fmt.Fprintf(w, "  Using %s\n", normalized)
	}
	baseURL = normalized
	if env.Has(fieldJiraEmail) {
		email = env.Get(fieldJiraEmail).Value
		env.Announce(w, fieldJiraEmail, "Atlassian email")
	} else if err := askText(w, "Atlassian email", "", &email, requireNonEmpty); err != nil {
		return err
	}
	if env.Has(fieldJiraToken) {
		jiraToken = env.Get(fieldJiraToken).Value
		env.Announce(w, fieldJiraToken, "Atlassian API token")
	} else if err := askPassword(w, "Atlassian API token", &jiraToken); err != nil {
		return err
	}

	fmt.Fprintln(w, "Validating Atlassian credentials...")
	jc := newJiraClient(baseURL, email, jiraToken)
	myself, err := withHTTPTimeout(ctx, func(ctx context.Context) (jiraMyself, error) {
		return jc.VerifyAuth(ctx)
	})
	if err != nil {
		return fmt.Errorf("Atlassian auth failed: %w", err)
	}
	fmt.Fprintf(w, "  OK — authenticated as %s (accountId %s)\n", myself.DisplayName, myself.AccountID)

	var projectsInput string
	if err := askText(w, "Jira project key(s), comma-separated (e.g. CD,ENG)", "", &projectsInput, requireNonEmpty); err != nil {
		return err
	}
	projects := splitCSV(projectsInput)
	fmt.Fprintln(w, "Validating project key(s)...")
	for _, p := range projects {
		_, err := withHTTPTimeout(ctx, func(ctx context.Context) (struct{}, error) {
			return struct{}{}, jc.VerifyProject(ctx, p)
		})
		if err != nil {
			return err
		}
		fmt.Fprintf(w, "  OK — %s\n", p)
	}

	fmt.Fprintln(w, "Discovering custom fields...")
	res, err := withHTTPTimeout(ctx, func(ctx context.Context) (FieldResolution, error) {
		return jc.DiscoverFields(ctx)
	})
	if err != nil {
		return fmt.Errorf("discover fields: %w", err)
	}

	storyPointsID, err := resolveFieldChoice(w, "Story Points", res.StoryPointsID, res.StoryPointsCandidates)
	if err != nil {
		return err
	}
	epicLinkID, err := resolveFieldChoice(w, "Epic Link", res.EpicLinkID, res.EpicLinkCandidates)
	if err != nil {
		return err
	}

	// --- GitHub ---
	fmt.Fprintln(w)
	fmt.Fprintln(w, "GitHub setup")
	fmt.Fprintln(w, "------------")

	var ghUsername, ghToken string
	if err := askText(w, "GitHub username", "", &ghUsername, requireNonEmpty); err != nil {
		return err
	}
	if env.Has(fieldGithubToken) {
		ghToken = env.Get(fieldGithubToken).Value
		env.Announce(w, fieldGithubToken, "GitHub personal access token")
	} else if err := askPassword(w, "GitHub personal access token", &ghToken); err != nil {
		return err
	}

	fmt.Fprintln(w, "Validating GitHub token...")
	gc := newGithubClient(ghToken)
	ghUser, err := withHTTPTimeout(ctx, func(ctx context.Context) (githubUser, error) {
		return gc.VerifyAuth(ctx)
	})
	if err != nil {
		return fmt.Errorf("GitHub auth failed: %w", err)
	}
	fmt.Fprintf(w, "  OK — authenticated as %s\n", ghUser.Login)
	if !strings.EqualFold(ghUser.Login, ghUsername) {
		fmt.Fprintf(w, "  Note: token belongs to %q but you entered %q. Using %q for config.\n", ghUser.Login, ghUsername, ghUser.Login)
		ghUsername = ghUser.Login
	}

	var orgsInput string
	if env.Has(fieldGithubOrgs) {
		orgsInput = env.Get(fieldGithubOrgs).Value
		env.Announce(w, fieldGithubOrgs, "GitHub org(s)")
	} else if err := askText(w, "GitHub org(s), comma-separated", "", &orgsInput, requireNonEmpty); err != nil {
		return err
	}
	orgs := splitCSV(orgsInput)
	fmt.Fprintln(w, "Validating org(s)...")
	for _, o := range orgs {
		_, err := withHTTPTimeout(ctx, func(ctx context.Context) (struct{}, error) {
			return struct{}{}, gc.VerifyOrgVisible(ctx, o)
		})
		if err != nil {
			return err
		}
		fmt.Fprintf(w, "  OK — %s\n", o)
	}

	// --- Commit ---
	// Preserve any [[devs]] mappings from an existing config so re-running
	// init to refresh creds doesn't wipe out manually-confirmed pairings.
	var preservedDevs []config.DevIdentity
	if existing, loadErr := config.Load(); loadErr == nil {
		preservedDevs = existing.ActiveProfile().Devs
		if len(preservedDevs) > 0 {
			ies := "ies"
			if len(preservedDevs) == 1 {
				ies = "y"
			}
			fmt.Fprintf(w, "Preserving %d existing [[devs]] entr%s from current config.\n", len(preservedDevs), ies)
		}
	}

	cfg := &config.Config{
		Profiles: map[string]config.Profile{
			config.DefaultProfile: func() config.Profile {
				p := config.DefaultProfileConfig()
				p.Jira = config.JiraConfig{
					BaseURL:   strings.TrimRight(baseURL, "/"),
					Email:     email,
					Projects:  projects,
					AccountID: myself.AccountID,
					Fields: config.JiraFields{
						StoryPoints: storyPointsID,
						EpicLink:    epicLinkID,
					},
				}
				p.GitHub = config.GitHubConfig{
					Username: ghUsername,
					Orgs:     orgs,
				}
				p.Devs = preservedDevs
				return p
			}(),
		},
	}

	if err := cfg.Save(); err != nil {
		return fmt.Errorf("save config: %w", err)
	}
	if err := secrets.Set(config.DefaultProfile, "jira", jiraToken); err != nil {
		return fmt.Errorf("store jira token: %w", err)
	}
	if err := secrets.Set(config.DefaultProfile, "github", ghToken); err != nil {
		return fmt.Errorf("store github token: %w", err)
	}

	// Materialize the SQLite cache now (the default and only standard backend)
	// so the substrate is ready before the first refresh and any disk/permission
	// problem surfaces during setup rather than mid-pull.
	store, err := cache.OpenStore()
	if err != nil {
		return fmt.Errorf("initialize cache: %w", err)
	}
	_ = store.Close()

	path, _ := config.Path()
	fmt.Fprintln(w)
	fmt.Fprintf(w, "Wrote config to %s\n", path)
	fmt.Fprintln(w, "Tokens stored in the OS keychain.")
	fmt.Fprintln(w, "Initialized the local SQLite cache.")
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Next steps:")
	fmt.Fprintln(w, "  1. velocity refresh --since 2019-11   # backfill history (pick any start month)")
	fmt.Fprintln(w, "  2. velocity devs discover             # build the contributor roster — REQUIRED for the leaderboard")
	fmt.Fprintln(w, "  3. velocity analyze                   # compute metrics from the cache")
	fmt.Fprintln(w, "  4. velocity serve --open              # open the dashboard")
	return nil
}

// resolveFieldChoice returns the field ID given what DiscoverFields found.
// - If autoID is set, use it.
// - If there are multiple candidates, prompt the user to pick.
// - Otherwise error.
func resolveFieldChoice(w io.Writer, label, autoID string, candidates []jiraField) (string, error) {
	if autoID != "" {
		if len(candidates) > 0 {
			fmt.Fprintf(w, "  %s → %s (%q)\n", label, autoID, candidates[0].Name)
		} else {
			fmt.Fprintf(w, "  %s → %s\n", label, autoID)
		}
		return autoID, nil
	}
	if len(candidates) == 0 {
		return "", fmt.Errorf("no %s field found on this Jira instance", label)
	}

	options := make([]string, len(candidates))
	for i, c := range candidates {
		options[i] = fmt.Sprintf("%s — %s", c.ID, c.Name)
	}
	var picked string
	if err := survey.AskOne(&survey.Select{
		Message: fmt.Sprintf("Multiple %s fields found. Pick one:", label),
		Options: options,
	}, &picked, survey.WithValidator(survey.Required)); err != nil {
		return "", err
	}
	// "id — name" → "id"
	id := strings.SplitN(picked, " — ", 2)[0]
	return id, nil
}

func askText(_ io.Writer, message, def string, out *string, validators ...survey.Validator) error {
	opts := []survey.AskOpt{}
	for _, v := range validators {
		opts = append(opts, survey.WithValidator(v))
	}
	return survey.AskOne(&survey.Input{Message: message + ":", Default: def}, out, opts...)
}

func askPassword(_ io.Writer, message string, out *string) error {
	return survey.AskOne(&survey.Password{Message: message + ":"}, out, survey.WithValidator(survey.Required))
}

func requireNonEmpty(ans interface{}) error {
	s, _ := ans.(string)
	if strings.TrimSpace(s) == "" {
		return fmt.Errorf("value required")
	}
	return nil
}

// NormalizeJiraURL accepts one of:
//   - a full URL ("https://foo.atlassian.net" / "http://localhost:8080")
//   - a hostname ("foo.atlassian.net")
//   - a bare subdomain ("foo") — expanded to "https://foo.atlassian.net"
//
// Returns the canonical form with scheme and no trailing slash. Rejects input
// with invalid URL characters or bad schemes.
func NormalizeJiraURL(raw string) (string, error) {
	s := strings.TrimSpace(raw)
	s = strings.TrimRight(s, "/")
	if s == "" {
		return "", fmt.Errorf("Atlassian URL is empty")
	}
	// Bare subdomain (single DNS label, no scheme) → expand to .atlassian.net.
	if !strings.Contains(s, "://") && !strings.Contains(s, ".") && !strings.Contains(s, ":") {
		s = "https://" + s + ".atlassian.net"
	} else if !strings.Contains(s, "://") {
		// Hostname given without scheme — assume https.
		s = "https://" + s
	}
	u, err := url.Parse(s)
	if err != nil {
		return "", fmt.Errorf("invalid Atlassian URL %q: %w", raw, err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("invalid Atlassian URL %q: scheme must be http or https", raw)
	}
	if u.Host == "" {
		return "", fmt.Errorf("invalid Atlassian URL %q: missing host", raw)
	}
	return strings.TrimRight(u.String(), "/"), nil
}

func splitCSV(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// withHTTPTimeout runs fn with a 20s context timeout so a hung network call
// doesn't freeze the CLI. Cancel always fires (via defer) so the context's
// internal goroutine doesn't leak.
//
// Go generics note: T lets one helper cover every API shape (jiraMyself,
// githubUser, FieldResolution, struct{} for void-returning calls) without
// losing type safety at the call site.
func withHTTPTimeout[T any](parent context.Context, fn func(context.Context) (T, error)) (T, error) {
	ctx, cancel := context.WithTimeout(parent, 20*time.Second)
	defer cancel()
	return fn(ctx)
}
