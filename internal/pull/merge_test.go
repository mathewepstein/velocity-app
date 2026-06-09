package pull

import (
	"testing"
	"time"

	"github.com/mathewepstein/velocity/internal/cache"
)

func TestMergeHydration(t *testing.T) {
	t0 := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	t1 := time.Date(2026, 6, 2, 0, 0, 0, 0, time.UTC)

	// A fully-hydrated cached record carries a description + raw fields.
	hydrated := func(key string, updated time.Time) cache.JiraIssue {
		return cache.JiraIssue{Key: key, Updated: updated, Description: "cached", DetailFetched: true, RawFields: []cache.RawField{{ID: "x"}}}
	}
	// A fresh base-pull record has neither (the detail stage fills them).
	base := func(key string, updated time.Time) cache.JiraIssue {
		return cache.JiraIssue{Key: key, Updated: updated}
	}

	existing := []cache.JiraIssue{
		hydrated("CD-1", t0),                              // unchanged updated → carry forward
		hydrated("CD-2", t0),                              // updated advances in fresh → re-hydrate
		hydrated("CD-3", t0),                              // not in fresh → dropped
		{Key: "CD-4", Updated: t0, DetailFetched: false},  // cached but not fully hydrated → don't carry
	}
	fresh := []cache.JiraIssue{
		base("CD-1", t0), // same updated
		base("CD-2", t1), // advanced
		base("CD-4", t0), // cached incomplete
		base("CD-5", t0), // new
	}

	out := mergeHydration(existing, fresh)

	if len(out) != 4 {
		t.Fatalf("want 4 (fresh set), got %d", len(out))
	}
	// Order follows fresh; CD-3 (cached-only) is dropped.
	wantKeys := []string{"CD-1", "CD-2", "CD-4", "CD-5"}
	for i, k := range wantKeys {
		if out[i].Key != k {
			t.Fatalf("out[%d].Key = %q, want %q", i, out[i].Key, k)
		}
	}
	// CD-1: carried — cached hydration preserved.
	if out[0].Description != "cached" || out[0].RawFields == nil {
		t.Errorf("CD-1 should carry forward cached hydration, got %#v", out[0])
	}
	// CD-2: updated advanced → fresh base record (no hydration).
	if out[1].Description != "" || out[1].RawFields != nil {
		t.Errorf("CD-2 should be the fresh base record, got %#v", out[1])
	}
	// CD-4: cached wasn't fully hydrated → fresh.
	if out[2].RawFields != nil {
		t.Errorf("CD-4 should not carry an unhydrated cached record, got %#v", out[2])
	}
}

func TestMergeHydration_NoExisting(t *testing.T) {
	fresh := []cache.JiraIssue{{Key: "CD-1"}, {Key: "CD-2"}}
	out := mergeHydration(nil, fresh)
	if len(out) != 2 || out[0].Key != "CD-1" || out[1].Key != "CD-2" {
		t.Fatalf("with no existing, output should equal fresh, got %#v", out)
	}
}
