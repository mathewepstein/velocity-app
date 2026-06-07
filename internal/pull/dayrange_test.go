package pull

import (
	"testing"
	"time"

	"github.com/mathewepstein/velocity/internal/cache"
)

func mustDay(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.Parse("2006-01-02", s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return v.UTC()
}

func TestDayRange_String(t *testing.T) {
	r := dayRange{Start: mustDay(t, "2024-05-01"), End: mustDay(t, "2024-05-31")}
	if got, want := r.String(), "2024-05-01..2024-05-31"; got != want {
		t.Fatalf("String() = %q, want %q", got, want)
	}
}

func TestDayRange_Days(t *testing.T) {
	cases := []struct {
		start, end string
		want       int
	}{
		{"2024-05-01", "2024-05-01", 1},
		{"2024-05-01", "2024-05-02", 2},
		{"2024-05-01", "2024-05-31", 31},
		{"2024-02-01", "2024-02-29", 29}, // leap
	}
	for _, c := range cases {
		r := dayRange{Start: mustDay(t, c.start), End: mustDay(t, c.end)}
		if got := r.Days(); got != c.want {
			t.Errorf("%s..%s Days() = %d, want %d", c.start, c.end, got, c.want)
		}
	}
}

func TestDayRange_Bisect_NoOverlapNoGap(t *testing.T) {
	r := dayRange{Start: mustDay(t, "2024-05-01"), End: mustDay(t, "2024-05-31")}
	left, right := r.Bisect()
	// 31 days → leftDays=15 → left=2024-05-01..2024-05-15, right=2024-05-16..2024-05-31
	if got, want := left.String(), "2024-05-01..2024-05-15"; got != want {
		t.Errorf("left = %q, want %q", got, want)
	}
	if got, want := right.String(), "2024-05-16..2024-05-31"; got != want {
		t.Errorf("right = %q, want %q", got, want)
	}
	// Adjacency: right.Start = left.End + 1 day. No overlap, no gap.
	if !right.Start.Equal(left.End.AddDate(0, 0, 1)) {
		t.Errorf("bisect halves are not adjacent: left.End=%s right.Start=%s", left.End, right.Start)
	}
	// Day counts sum back to the parent.
	if left.Days()+right.Days() != r.Days() {
		t.Errorf("Days sum: %d + %d != %d", left.Days(), right.Days(), r.Days())
	}
}

func TestDayRange_Bisect_TwoDays(t *testing.T) {
	r := dayRange{Start: mustDay(t, "2024-05-01"), End: mustDay(t, "2024-05-02")}
	left, right := r.Bisect()
	if got, want := left.String(), "2024-05-01..2024-05-01"; got != want {
		t.Errorf("left = %q, want %q", got, want)
	}
	if got, want := right.String(), "2024-05-02..2024-05-02"; got != want {
		t.Errorf("right = %q, want %q", got, want)
	}
}

func TestMonthRange(t *testing.T) {
	m, err := cache.ParseMonth("2024-02")
	if err != nil {
		t.Fatal(err)
	}
	r := monthRange(m)
	// February 2024 is a leap year: 29 days.
	if got, want := r.String(), "2024-02-01..2024-02-29"; got != want {
		t.Errorf("monthRange(2024-02) = %q, want %q", got, want)
	}
}
