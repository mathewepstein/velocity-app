package pull

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/mathewepstein/velocity/internal/config"
)

func TestStoryPointsFieldIDs(t *testing.T) {
	settable := map[string]editMetaField{
		"customfield_11102": {name: "Story point estimate", custom: "com.pyxis.greenhopper.jira:jsw-story-points"},
		"customfield_10128": {name: "Story Points", custom: ""},
		"summary":           {name: "Summary"},
		"customfield_9999":  {name: "Sprint Points", custom: "x:story-points"}, // matched by schema, not name
	}
	got := storyPointsFieldIDs(settable)
	want := []string{"customfield_10128", "customfield_11102", "customfield_9999"} // sorted; "summary" excluded
	if strings.Join(got, "|") != strings.Join(want, "|") {
		t.Errorf("storyPointsFieldIDs = %v, want %v", got, want)
	}
}

// editmetaServer serves a single issue's editmeta with the given settable
// fields (field ID → schema.custom), each named after its ID with a "set" op.
func editmetaServer(t *testing.T, settable map[string]string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("GET /rest/api/3/issue/{key}/editmeta", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		var b strings.Builder
		b.WriteString(`{"fields":{`)
		first := true
		for id, custom := range settable {
			if !first {
				b.WriteString(",")
			}
			first = false
			b.WriteString(`"` + id + `":{"name":"` + id + `","schema":{"custom":"` + custom + `"},"operations":["set"]}`)
		}
		b.WriteString(`}}`)
		_, _ = w.Write([]byte(b.String()))
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func writerFor(baseURL, overrideField string) *JiraWriter {
	return NewJiraWriter(config.JiraConfig{
		BaseURL: baseURL, Email: "t@example.com",
		Fields: config.JiraFields{StoryPoints: overrideField},
	}, "token")
}

func TestResolveStoryPointsField_AutoResolvesIgnoringStaleOverride(t *testing.T) {
	// The configured field is NOT on the edit screen; one real SP field is.
	// Resolution must ignore the stale override and find the settable field —
	// the whole point of the fix.
	srv := editmetaServer(t, map[string]string{"customfield_11102": "x:story-points", "summary": ""})
	w := writerFor(srv.URL, "customfield_10128")

	got, err := w.resolveStoryPointsField(context.Background(), "CD-1")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if got != "customfield_11102" {
		t.Errorf("resolved %q, want customfield_11102", got)
	}
}

func TestResolveStoryPointsField_HonorsValidOverride(t *testing.T) {
	// Two settable SP fields; a valid override pins which one to use.
	srv := editmetaServer(t, map[string]string{
		"customfield_11102": "x:story-points",
		"customfield_10128": "x:story-points",
	})
	w := writerFor(srv.URL, "customfield_10128")

	got, err := w.resolveStoryPointsField(context.Background(), "CD-1")
	if err != nil || got != "customfield_10128" {
		t.Fatalf("resolved %q (err %v), want override customfield_10128", got, err)
	}
}

func TestResolveStoryPointsField_NoConfigStillResolves(t *testing.T) {
	// Empty override: a fresh user who never mapped the field can still post.
	srv := editmetaServer(t, map[string]string{"customfield_11102": "x:story-points"})
	w := writerFor(srv.URL, "")

	got, err := w.resolveStoryPointsField(context.Background(), "CD-1")
	if err != nil || got != "customfield_11102" {
		t.Fatalf("resolved %q (err %v), want customfield_11102", got, err)
	}
}

func TestResolveStoryPointsField_NoneSettableIsActionableError(t *testing.T) {
	srv := editmetaServer(t, map[string]string{"summary": "", "description": ""})
	w := writerFor(srv.URL, "customfield_10128")

	_, err := w.resolveStoryPointsField(context.Background(), "CD-1")
	if err == nil || !strings.Contains(err.Error(), "no Story Points field is settable") {
		t.Fatalf("want a 'no Story Points field settable' error, got %v", err)
	}
	if !strings.Contains(err.Error(), "customfield_10128") {
		t.Errorf("error should mention the misconfigured field: %v", err)
	}
}

func TestResolveStoryPointsField_AmbiguousIsActionableError(t *testing.T) {
	// Multiple settable SP fields and no override → ask the operator to pick,
	// naming the candidates rather than guessing.
	srv := editmetaServer(t, map[string]string{
		"customfield_11102": "x:story-points",
		"customfield_10128": "x:story-points",
	})
	w := writerFor(srv.URL, "")

	_, err := w.resolveStoryPointsField(context.Background(), "CD-1")
	if err == nil || !strings.Contains(err.Error(), "multiple settable Story Points fields") {
		t.Fatalf("want ambiguity error, got %v", err)
	}
	if !strings.Contains(err.Error(), "customfield_11102") || !strings.Contains(err.Error(), "customfield_10128") {
		t.Errorf("ambiguity error should name both candidates: %v", err)
	}
}

func TestResolveStoryPointsField_EditmetaUnreadableFallsBackToOverride(t *testing.T) {
	// editmeta unreadable (404, not retried) → trust the config hint if present.
	mux := http.NewServeMux()
	mux.HandleFunc("GET /rest/api/3/issue/{key}/editmeta", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	got, err := writerFor(srv.URL, "customfield_10128").resolveStoryPointsField(context.Background(), "CD-1")
	if err != nil || got != "customfield_10128" {
		t.Fatalf("resolved %q (err %v), want override fallback customfield_10128", got, err)
	}

	// No override either → explicit error, not a silent empty write.
	if _, err := writerFor(srv.URL, "").resolveStoryPointsField(context.Background(), "CD-1"); err == nil {
		t.Error("want error when editmeta unreadable and no override set")
	}
}
