package jirafields

import "testing"

func TestIsPopulated(t *testing.T) {
	cases := []struct {
		name string
		v    interface{}
		want bool
	}{
		{"nil", nil, false},
		{"empty string", "", false},
		{"blank string", "   ", false},
		{"text", "hi", true},
		{"false", false, false},
		{"true", true, true},
		{"zero", float64(0), false},
		{"number", float64(3), true},
		{"empty array", []interface{}{}, false},
		{"array", []interface{}{1}, true},
		{"empty object", map[string]interface{}{}, false},
		{"object", map[string]interface{}{"k": 1}, true},
	}
	for _, c := range cases {
		if got := isPopulated(c.v); got != c.want {
			t.Errorf("%s: isPopulated=%v want %v", c.name, got, c.want)
		}
	}
}

func TestShapeOf(t *testing.T) {
	cases := []struct {
		v    interface{}
		want string
	}{
		{"s", "string"},
		{float64(1), "number"},
		{true, "bool"},
		{[]interface{}{1, 2}, "array[2]"},
		{map[string]interface{}{"b": 1, "a": 2}, "object{a,b}"},
	}
	for _, c := range cases {
		if got := shapeOf(c.v); got != c.want {
			t.Errorf("shapeOf(%v)=%q want %q", c.v, got, c.want)
		}
	}
}

// catalog used across buildReport tests: a CD-shaped instance where the real
// description lives in a custom field and the standard description is sparse.
func testCatalog() []FieldMeta {
	return []FieldMeta{
		{ID: "description", Name: "Description", Custom: false},
		{ID: "customfield_11140", Name: "Description - V2", Custom: true},
		{ID: "customfield_11102", Name: "Story point estimate", Custom: true},
		{ID: "parent", Name: "Parent", Custom: false},
		{ID: "customfield_10126", Name: "Flagged", Custom: true},
		{ID: "customfield_11000", Name: "Request Type", Custom: true},
		{ID: "labels", Name: "Labels", Custom: false},
	}
}

func TestBuildReport_DescriptionPicksMostPopulatedCustom(t *testing.T) {
	// Standard description populated once; the custom V2 field populated on all
	// three — the CD reality. Proposal must pick the custom field.
	issues := []map[string]interface{}{
		{"description": "only here", "customfield_11140": "real", "customfield_11102": float64(3)},
		{"customfield_11140": "real", "customfield_11102": float64(5)},
		{"customfield_11140": "real"},
	}
	rep := buildReport(testCatalog(), []string{"CD-1", "CD-2", "CD-3"}, issues)

	desc := proposalFor(rep, "description")
	if desc == nil {
		t.Fatal("no description proposal")
	}
	if desc.FieldID != "customfield_11140" {
		t.Errorf("description proposed %q, want customfield_11140", desc.FieldID)
	}
}

func TestBuildReport_StoryPointsAndEpicFallback(t *testing.T) {
	issues := []map[string]interface{}{
		{"customfield_11102": float64(8)},
	}
	rep := buildReport(testCatalog(), []string{"CD-1"}, issues)

	if sp := proposalFor(rep, "story_points"); sp == nil || sp.FieldID != "customfield_11102" {
		t.Errorf("story_points proposal = %+v, want customfield_11102", sp)
	}
	// No custom "Epic Link" field in the catalog → must fall back to parent.
	if el := proposalFor(rep, "epic_link"); el == nil || el.FieldID != "parent" {
		t.Errorf("epic_link proposal = %+v, want parent fallback", el)
	}
}

func TestBuildReport_StoryPointsProposedEvenWhenUnpopulated(t *testing.T) {
	// The story-points field exists in the catalog but is empty on every sampled
	// (recent/open) ticket. It must still be proposed — matched via the catalog,
	// not the population stats. Regression for the live smoke-test gap.
	issues := []map[string]interface{}{
		{"customfield_11140": "desc only"},
	}
	rep := buildReport(testCatalog(), []string{"CD-1"}, issues)
	sp := proposalFor(rep, "story_points")
	if sp == nil || sp.FieldID != "customfield_11102" {
		t.Fatalf("story_points proposal = %+v, want customfield_11102 from catalog", sp)
	}
}

func TestBuildReport_NoiseDenylistedAndExtraBucketed(t *testing.T) {
	issues := []map[string]interface{}{
		{
			"customfield_10126": []interface{}{map[string]interface{}{"value": "Impediment"}}, // Flagged → extra
			"customfield_11000": map[string]interface{}{"name": "Bug"},                         // Request Type → noise
			"labels":            []interface{}{"BE"},                                            // standard, not custom → neither
		},
	}
	rep := buildReport(testCatalog(), []string{"CD-1"}, issues)

	if !containsID(rep.Extra, "customfield_10126") {
		t.Error("Flagged should be bucketed into Extra (capture-worthy custom field)")
	}
	if !containsID(rep.Denylisted, "customfield_11000") {
		t.Error("Request Type should be denylisted as noise")
	}
	if containsID(rep.Extra, "labels") || containsID(rep.Denylisted, "labels") {
		t.Error("standard field 'labels' should not appear in Extra or Denylisted")
	}
}

func TestBuildReport_StatsSortedAndCounted(t *testing.T) {
	issues := []map[string]interface{}{
		{"customfield_11140": "a", "labels": []interface{}{"x"}},
		{"customfield_11140": "b"},
	}
	rep := buildReport(testCatalog(), []string{"CD-1", "CD-2"}, issues)
	if len(rep.Stats) == 0 || rep.Stats[0].ID != "customfield_11140" {
		t.Fatalf("expected most-populated field first, got %+v", rep.Stats)
	}
	if rep.Stats[0].Populated != 2 || rep.Stats[0].Sampled != 2 {
		t.Errorf("description stat = %d/%d, want 2/2", rep.Stats[0].Populated, rep.Stats[0].Sampled)
	}
}

func proposalFor(r *Report, canonical string) *Proposal {
	for i := range r.Proposed {
		if r.Proposed[i].Canonical == canonical {
			return &r.Proposed[i]
		}
	}
	return nil
}

func containsID(stats []FieldStat, id string) bool {
	for _, s := range stats {
		if s.ID == id {
			return true
		}
	}
	return false
}
