package cache

import (
	"errors"
	"io/fs"
	"testing"
	"time"
)

func TestWriteReadMonth_Roundtrip(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	month := MustParseMonth("2024-01")
	in := []JiraIssue{
		{
			Key:         "CD-1",
			Summary:     "first issue",
			Status:      "Done",
			Created:     time.Date(2024, 1, 5, 0, 0, 0, 0, time.UTC),
			Updated:     time.Date(2024, 1, 10, 0, 0, 0, 0, time.UTC),
			StoryPoints: 3,
			EpicKey:     "CD-100",
			Labels:      []string{"backend", "urgent"},
		},
		{Key: "CD-2", Summary: "second", Status: "In Progress"},
	}

	if err := WriteMonth(SourceJira, "CD", month, in); err != nil {
		t.Fatalf("Write: %v", err)
	}
	out, err := ReadMonth[JiraIssue](SourceJira, "CD", month)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if len(out) != 2 {
		t.Fatalf("records = %d, want 2", len(out))
	}
	if out[0].Key != "CD-1" || out[0].StoryPoints != 3 {
		t.Errorf("record[0] = %+v", out[0])
	}
	if len(out[0].Labels) != 2 {
		t.Errorf("labels = %v", out[0].Labels)
	}
}

func TestReadMonth_MissingIsNotExist(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	_, err := ReadMonth[JiraIssue](SourceJira, "CD", MustParseMonth("2099-12"))
	if err == nil {
		t.Fatal("expected error for missing file")
	}
	if !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("expected fs.ErrNotExist wrapped, got %v", err)
	}
}

func TestWriteMonth_EmptySliceWritesBrackets(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	if err := WriteMonth[JiraIssue](SourceJira, "CD", MustParseMonth("2024-01"), nil); err != nil {
		t.Fatalf("Write: %v", err)
	}
	out, err := ReadMonth[JiraIssue](SourceJira, "CD", MustParseMonth("2024-01"))
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if out == nil {
		t.Error("expected non-nil empty slice on round-trip of nil write")
	}
	if len(out) != 0 {
		t.Errorf("expected empty, got %d entries", len(out))
	}
}

func TestMonthPath_RejectsBadScope(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	cases := []string{"../evil", "a/b", "/abs"}
	for _, bad := range cases {
		if _, err := MonthPath(SourceJira, bad, MustParseMonth("2024-01")); err == nil {
			t.Errorf("MonthPath accepted bad scope %q", bad)
		}
	}
}

func TestWriteMonth_AtomicNoTmpLeaks(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	month := MustParseMonth("2024-01")
	if err := WriteMonth(SourceJira, "CD", month, []JiraIssue{{Key: "CD-1"}}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	// Sanity: the tmp sibling should be gone after success.
	path, _ := MonthPath(SourceJira, "CD", month)
	if _, err := fsStat(path + ".tmp"); err == nil {
		t.Error("tmp file lingered after successful write")
	}
}

// fsStat wraps os.Stat so the test file stays import-narrow.
func fsStat(path string) (any, error) {
	return statFn(path)
}
