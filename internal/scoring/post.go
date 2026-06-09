package scoring

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// JiraPoster is the write surface PostScores needs. The concrete implementation
// (pull.JiraWriter) lives in internal/pull; the interface is defined here so
// PostScores can be tested with a fake and so internal/scoring never imports the
// HTTP layer (consumer-defined interface).
type JiraPoster interface {
	SetStoryPoints(ctx context.Context, key string, points float64) error
	AddComment(ctx context.Context, key string, lines []string) error
	VerifyWriteScope(ctx context.Context) error
}

// PostOptions controls a PostScores run.
type PostOptions struct {
	Tickets []string // explicit ticket keys to post
	DryRun  bool     // when true, build comments + report but never touch Jira
	Scorer  string   // "" → ScorerID
}

// PostAction is one ticket's outcome.
type PostAction string

const (
	ActionPosted        PostAction = "posted"         // SP + comment written, row marked
	ActionPreview       PostAction = "preview"        // dry-run; nothing written
	ActionAlreadyPosted PostAction = "already_posted" // idempotent skip
	ActionNoRow         PostAction = "no_score"       // no persisted score for the ticket
	ActionError         PostAction = "error"          // write failed mid-ticket
)

// PostResult is the per-ticket outcome, including the comment that was (or in a
// dry run, would be) posted so the caller can preview it.
type PostResult struct {
	Ticket  string     `json:"ticket"`
	Points  int        `json:"points"`
	Action  PostAction `json:"action"`
	Comment []string   `json:"comment,omitempty"`
	Error   string     `json:"error,omitempty"`
}

// PostReport summarizes a run. The per-action counts let a caller render a
// one-line tally without re-walking Results.
type PostReport struct {
	DryRun        bool         `json:"dry_run"`
	Results       []PostResult `json:"results"`
	Posted        int          `json:"posted"`
	Previewed     int          `json:"previewed"`
	AlreadyPosted int          `json:"already_posted"`
	NoRow         int          `json:"no_score"`
	Errors        int          `json:"errors"`
}

// PostScores writes the persisted score for each requested ticket back to Jira
// — the Story Points field plus one calibration comment — then marks the row
// posted. It is idempotent (a row already posted_to_jira is skipped) and honors
// DryRun (builds the comment + report but performs no writes). One ticket's
// failure is recorded in its result and does not abort the batch. The field
// write happens before the comment; a comment failure after a successful field
// write is reported as such (the row is NOT marked posted, so a re-run retries).
func PostScores(ctx context.Context, store *ScoreStore, poster JiraPoster, opts PostOptions) (PostReport, error) {
	if store == nil {
		return PostReport{}, fmt.Errorf("score store is nil")
	}
	if !opts.DryRun && poster == nil {
		return PostReport{}, fmt.Errorf("no jira poster configured (cannot post live)")
	}
	scorer := orDefault(opts.Scorer, ScorerID)
	rep := PostReport{DryRun: opts.DryRun}

	for _, ticket := range opts.Tickets {
		rec, ok, err := store.Get(ticket, scorer)
		if err != nil {
			rep.Results = append(rep.Results, PostResult{Ticket: ticket, Action: ActionError, Error: err.Error()})
			rep.Errors++
			continue
		}
		if !ok {
			rep.Results = append(rep.Results, PostResult{Ticket: ticket, Action: ActionNoRow})
			rep.NoRow++
			continue
		}

		comment := BuildComment(*rec)
		res := PostResult{Ticket: ticket, Points: rec.Points, Comment: comment}

		switch {
		case rec.PostedToJira:
			res.Action = ActionAlreadyPosted
			rep.AlreadyPosted++
		case opts.DryRun:
			res.Action = ActionPreview
			rep.Previewed++
		default:
			res.Action, res.Error = postOne(ctx, store, poster, ticket, scorer, rec.Points, comment)
			switch res.Action {
			case ActionPosted:
				rep.Posted++
			case ActionError:
				rep.Errors++
			}
		}
		rep.Results = append(rep.Results, res)
	}
	return rep, nil
}

// postOne performs the live write for a single ticket: Story Points field, then
// comment, then mark-posted. Returns the resulting action + an error message
// (empty on success). Kept separate so the per-ticket failure handling reads
// linearly instead of nesting in the PostScores loop.
func postOne(ctx context.Context, store *ScoreStore, poster JiraPoster, ticket, scorer string, points int, comment []string) (PostAction, string) {
	if err := poster.SetStoryPoints(ctx, ticket, float64(points)); err != nil {
		return ActionError, err.Error()
	}
	if err := poster.AddComment(ctx, ticket, comment); err != nil {
		return ActionError, fmt.Sprintf("story points set but comment failed (re-run to retry): %v", err)
	}
	if err := store.MarkPosted(ticket, scorer, time.Now()); err != nil {
		return ActionError, fmt.Sprintf("posted to jira but failed to mark the row: %v", err)
	}
	return ActionPosted, ""
}

// BuildComment renders the calibration comment for a score row, mirroring the
// /score-ticket Step 6 format: final points (noting an override), the hardest
// aspect, up to three drivers, the one-line signal summary, and the Clauditor
// signature. Each returned element is one line; "---" renders as a horizontal
// rule (see linesToADF in internal/pull).
func BuildComment(rec ScoreRecord) []string {
	var lines []string
	if rec.Source == SourceHuman && rec.Points != rec.AutoPoints {
		lines = append(lines, fmt.Sprintf("Story points: %d (velocity proposed %d; human override)", rec.Points, rec.AutoPoints))
	} else {
		lines = append(lines, fmt.Sprintf("Story points: %d", rec.Points))
	}
	if rec.HardestAspect != "" {
		lines = append(lines, "Hardest aspect: "+rec.HardestAspect)
	}
	// The band engine often sets HardestAspect to its top driver, so drop any
	// driver equal to the hardest-aspect line to avoid a verbatim repeat in the
	// comment. Cap the remainder at three.
	drivers := make([]string, 0, len(rec.Drivers))
	for _, d := range rec.Drivers {
		if strings.TrimSpace(d) == strings.TrimSpace(rec.HardestAspect) {
			continue
		}
		drivers = append(drivers, d)
		if len(drivers) == 3 {
			break
		}
	}
	if len(drivers) > 0 {
		lines = append(lines, "Top drivers:")
		for i, d := range drivers {
			lines = append(lines, fmt.Sprintf("%d. %s", i+1, d))
		}
	}
	if rec.SignalSummary != "" {
		lines = append(lines, "Signals: "+rec.SignalSummary)
	}
	lines = append(lines, "---")
	lines = append(lines, "Automated review by Clauditor — verified by a human reviewer before posting.")
	return lines
}
