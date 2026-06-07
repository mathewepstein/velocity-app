package pull

import (
	"encoding/json"
	"reflect"
	"testing"
)

// adf parses a JSON ADF literal into the decoded-interface shape flattenADF
// expects (what encoding/json produces for the Jira response).
func adf(t *testing.T, jsonDoc string) interface{} {
	t.Helper()
	var v interface{}
	if err := json.Unmarshal([]byte(jsonDoc), &v); err != nil {
		t.Fatalf("bad ADF literal: %v", err)
	}
	return v
}

func TestFlattenADF_SimpleParagraph(t *testing.T) {
	doc := adf(t, `{"type":"doc","content":[
		{"type":"paragraph","content":[{"type":"text","text":"Hello world"}]}
	]}`)
	got := flattenADF(doc)
	if got.Text != "Hello world" {
		t.Errorf("Text = %q, want %q", got.Text, "Hello world")
	}
	if len(got.URLs) != 0 {
		t.Errorf("URLs = %v, want none", got.URLs)
	}
}

func TestFlattenADF_MultipleBlocksSeparated(t *testing.T) {
	doc := adf(t, `{"type":"doc","content":[
		{"type":"heading","content":[{"type":"text","text":"Title"}]},
		{"type":"paragraph","content":[{"type":"text","text":"First."}]},
		{"type":"paragraph","content":[{"type":"text","text":"Second."}]}
	]}`)
	got := flattenADF(doc)
	want := "Title\nFirst.\nSecond."
	if got.Text != want {
		t.Errorf("Text = %q, want %q", got.Text, want)
	}
}

func TestFlattenADF_LinkMarkCapturesHref(t *testing.T) {
	doc := adf(t, `{"type":"doc","content":[
		{"type":"paragraph","content":[
			{"type":"text","text":"see the "},
			{"type":"text","text":"design doc","marks":[
				{"type":"link","attrs":{"href":"https://figma.com/file/abc"}}
			]}
		]}
	]}`)
	got := flattenADF(doc)
	if got.Text != "see the design doc" {
		t.Errorf("Text = %q", got.Text)
	}
	if !reflect.DeepEqual(got.URLs, []string{"https://figma.com/file/abc"}) {
		t.Errorf("URLs = %v, want [https://figma.com/file/abc]", got.URLs)
	}
}

func TestFlattenADF_InlineCardURL(t *testing.T) {
	doc := adf(t, `{"type":"doc","content":[
		{"type":"paragraph","content":[
			{"type":"inlineCard","attrs":{"url":"https://consumerdirect.atlassian.net/wiki/x/1"}}
		]}
	]}`)
	got := flattenADF(doc)
	if !reflect.DeepEqual(got.URLs, []string{"https://consumerdirect.atlassian.net/wiki/x/1"}) {
		t.Errorf("URLs = %v", got.URLs)
	}
}

func TestFlattenADF_BareURLInText(t *testing.T) {
	doc := adf(t, `{"type":"doc","content":[
		{"type":"paragraph","content":[
			{"type":"text","text":"plan: https://example.com/plan."}
		]}
	]}`)
	got := flattenADF(doc)
	// Trailing sentence period must be stripped from the extracted URL.
	if !reflect.DeepEqual(got.URLs, []string{"https://example.com/plan"}) {
		t.Errorf("URLs = %v, want [https://example.com/plan]", got.URLs)
	}
}

func TestFlattenADF_DedupesURLAcrossMarkAndText(t *testing.T) {
	doc := adf(t, `{"type":"doc","content":[
		{"type":"paragraph","content":[
			{"type":"text","text":"https://x.io/a","marks":[
				{"type":"link","attrs":{"href":"https://x.io/a"}}
			]}
		]}
	]}`)
	got := flattenADF(doc)
	if len(got.URLs) != 1 || got.URLs[0] != "https://x.io/a" {
		t.Errorf("URLs = %v, want exactly [https://x.io/a]", got.URLs)
	}
}

func TestFlattenADF_MentionAndHardBreak(t *testing.T) {
	doc := adf(t, `{"type":"doc","content":[
		{"type":"paragraph","content":[
			{"type":"mention","attrs":{"id":"123","text":"@Mathew"}},
			{"type":"text","text":" please review"},
			{"type":"hardBreak"},
			{"type":"text","text":"thanks"}
		]}
	]}`)
	got := flattenADF(doc)
	want := "@Mathew please review\nthanks"
	if got.Text != want {
		t.Errorf("Text = %q, want %q", got.Text, want)
	}
}

func TestFlattenADF_BulletList(t *testing.T) {
	doc := adf(t, `{"type":"doc","content":[
		{"type":"bulletList","content":[
			{"type":"listItem","content":[{"type":"paragraph","content":[{"type":"text","text":"one"}]}]},
			{"type":"listItem","content":[{"type":"paragraph","content":[{"type":"text","text":"two"}]}]}
		]}
	]}`)
	got := flattenADF(doc)
	want := "one\ntwo"
	if got.Text != want {
		t.Errorf("Text = %q, want %q", got.Text, want)
	}
}

func TestFlattenADF_NilAndEmpty(t *testing.T) {
	if got := flattenADF(nil); got.Text != "" || len(got.URLs) != 0 {
		t.Errorf("nil doc = %+v, want empty", got)
	}
	if got := flattenADF("not an object"); got.Text != "" || len(got.URLs) != 0 {
		t.Errorf("string doc = %+v, want empty", got)
	}
	empty := adf(t, `{"type":"doc","content":[]}`)
	if got := flattenADF(empty); got.Text != "" || len(got.URLs) != 0 {
		t.Errorf("empty doc = %+v, want empty", got)
	}
}
