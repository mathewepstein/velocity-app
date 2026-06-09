package pull

import (
	"testing"

	"github.com/mathewepstein/velocity/internal/cache"
)

func TestSetRelations(t *testing.T) {
	f := map[string]interface{}{
		"subtasks": []interface{}{
			map[string]interface{}{"key": "CD-2", "fields": map[string]interface{}{
				"status":    map[string]interface{}{"name": "Done"},
				"issuetype": map[string]interface{}{"name": "Sub-task"},
			}},
		},
		"issuelinks": []interface{}{
			map[string]interface{}{
				"type":         map[string]interface{}{"name": "Cloners", "inward": "is cloned by", "outward": "clones"},
				"outwardIssue": map[string]interface{}{"key": "CD-3", "fields": map[string]interface{}{"status": map[string]interface{}{"name": "Open"}}},
			},
			map[string]interface{}{
				"type":        map[string]interface{}{"name": "Blocks", "inward": "is blocked by", "outward": "blocks"},
				"inwardIssue": map[string]interface{}{"key": "CD-4"},
			},
		},
		"attachment": []interface{}{
			map[string]interface{}{"filename": "d.png", "mimeType": "image/png", "size": float64(99), "created": "2026-03-01T00:00:00.000-0700", "author": map[string]interface{}{"accountId": "acc1"}},
		},
		"fixVersions": []interface{}{
			map[string]interface{}{"name": "1.20"},
			map[string]interface{}{"name": "1.21"},
		},
	}
	var iss cache.JiraIssue
	setRelations(&iss, f)

	if len(iss.Links) != 3 {
		t.Fatalf("want 3 links (1 subtask + 2 issuelinks), got %d: %#v", len(iss.Links), iss.Links)
	}
	if iss.Links[0].LinkType != "subtask" || iss.Links[0].Key != "CD-2" || iss.Links[0].Status != "Done" {
		t.Errorf("subtask link wrong: %#v", iss.Links[0])
	}
	if iss.Links[1].LinkType != "Cloners" || iss.Links[1].Direction != "outward" || iss.Links[1].Phrase != "clones" || iss.Links[1].Key != "CD-3" {
		t.Errorf("outward link wrong: %#v", iss.Links[1])
	}
	if iss.Links[2].Direction != "inward" || iss.Links[2].Phrase != "is blocked by" || iss.Links[2].Key != "CD-4" {
		t.Errorf("inward link wrong: %#v", iss.Links[2])
	}
	if len(iss.Attachments) != 1 || iss.Attachments[0].Filename != "d.png" || iss.Attachments[0].MimeType != "image/png" || iss.Attachments[0].Size != 99 || iss.Attachments[0].Author != "acc1" {
		t.Errorf("attachment wrong: %#v", iss.Attachments)
	}
	if len(iss.FixVersions) != 2 || iss.FixVersions[0] != "1.20" {
		t.Errorf("fix versions wrong: %#v", iss.FixVersions)
	}
}

func TestSetRelations_EmptyIsNonNilSentinel(t *testing.T) {
	var iss cache.JiraIssue
	setRelations(&iss, map[string]interface{}{})
	if iss.Links == nil || len(iss.Links) != 0 {
		t.Errorf("Links should be non-nil empty (captured sentinel), got %#v", iss.Links)
	}
	if iss.Attachments == nil || len(iss.Attachments) != 0 {
		t.Errorf("Attachments should be non-nil empty, got %#v", iss.Attachments)
	}
	if iss.FixVersions != nil {
		t.Errorf("FixVersions should be nil when empty (omitempty), got %#v", iss.FixVersions)
	}
}

func TestBuildRawFields(t *testing.T) {
	fields := map[string]interface{}{
		"customfield_10126": []interface{}{map[string]interface{}{"value": "Impediment"}}, // populated, keep
		"priority":          map[string]interface{}{"name": "High"},                        // populated, keep
		"customfield_11000": map[string]interface{}{"name": "Bug"},                          // populated but noise → drop
		"resolution":        nil,                                                            // unpopulated → drop
		"workratio":         float64(0),                                                     // zero → drop
		"summary":           "hello",                                                        // populated, keep
	}
	names := map[string]string{
		"customfield_10126": "Flagged",
		"priority":          "Priority",
		"customfield_11000": "Request Type", // noise keyword
		"summary":           "Summary",
	}
	got := buildRawFields(fields, names)

	have := map[string]cache.RawField{}
	for _, rf := range got {
		have[rf.ID] = rf
	}
	if _, ok := have["customfield_11000"]; ok {
		t.Error("Request Type (noise) should be excluded from raw catch-all")
	}
	if _, ok := have["resolution"]; ok {
		t.Error("null field should be excluded")
	}
	if _, ok := have["workratio"]; ok {
		t.Error("zero numeric field should be excluded")
	}
	flagged, ok := have["customfield_10126"]
	if !ok || flagged.Name != "Flagged" || flagged.Value != `[{"value":"Impediment"}]` {
		t.Errorf("Flagged raw field wrong: %#v", flagged)
	}
	if pr, ok := have["priority"]; !ok || pr.Value != `{"name":"High"}` {
		t.Errorf("priority raw field wrong: %#v", pr)
	}
	// Deterministic ID order.
	for i := 1; i < len(got); i++ {
		if got[i-1].ID > got[i].ID {
			t.Errorf("raw fields not sorted by ID: %q before %q", got[i-1].ID, got[i].ID)
		}
	}
}
