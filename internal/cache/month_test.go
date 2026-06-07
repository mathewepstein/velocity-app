package cache

import (
	"testing"
	"time"
)

func TestParseMonth(t *testing.T) {
	tests := []struct {
		in      string
		wantErr bool
		want    Month
	}{
		{"2024-01", false, Month{2024, time.January}},
		{"2019-11", false, Month{2019, time.November}},
		{"2026-12", false, Month{2026, time.December}},
		{"2024-1", true, Month{}},  // month must be zero-padded
		{"24-01", true, Month{}},   // year must be four digits
		{"2024/01", true, Month{}}, // wrong separator
		{"2024-13", true, Month{}}, // month > 12
		{"", true, Month{}},
	}
	for _, tc := range tests {
		got, err := ParseMonth(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("ParseMonth(%q): expected error, got %+v", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("ParseMonth(%q): unexpected error: %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ParseMonth(%q) = %+v, want %+v", tc.in, got, tc.want)
		}
	}
}

func TestMonthString(t *testing.T) {
	tests := []struct {
		m    Month
		want string
	}{
		{Month{2024, time.January}, "2024-01"},
		{Month{2019, time.November}, "2019-11"},
		{Month{2026, time.December}, "2026-12"},
	}
	for _, tc := range tests {
		if got := tc.m.String(); got != tc.want {
			t.Errorf("%+v.String() = %q, want %q", tc.m, got, tc.want)
		}
	}
}

func TestMonthAdd(t *testing.T) {
	tests := []struct {
		m    Month
		n    int
		want Month
	}{
		{Month{2024, time.January}, 1, Month{2024, time.February}},
		{Month{2024, time.December}, 1, Month{2025, time.January}},  // year rollover
		{Month{2024, time.January}, -1, Month{2023, time.December}}, // back rollover
		{Month{2024, time.January}, 13, Month{2025, time.February}}, // multi-year
		{Month{2024, time.January}, 0, Month{2024, time.January}},
	}
	for _, tc := range tests {
		if got := tc.m.Add(tc.n); got != tc.want {
			t.Errorf("%+v.Add(%d) = %+v, want %+v", tc.m, tc.n, got, tc.want)
		}
	}
}

func TestMonthsInRange(t *testing.T) {
	start := MustParseMonth("2024-10")
	end := MustParseMonth("2025-02")
	got := MonthsInRange(start, end)
	want := []Month{
		{2024, time.October},
		{2024, time.November},
		{2024, time.December},
		{2025, time.January},
		{2025, time.February},
	}
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d", len(got), len(want))
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("[%d] = %+v, want %+v", i, got[i], want[i])
		}
	}

	// Reversed range → nil.
	if got := MonthsInRange(end, start); got != nil {
		t.Errorf("reversed range: expected nil, got %v", got)
	}
	// Single-month range.
	single := MonthsInRange(start, start)
	if len(single) != 1 || single[0] != start {
		t.Errorf("single-month range: %v", single)
	}
}

func TestCurrentMonth(t *testing.T) {
	now := time.Date(2024, time.March, 15, 10, 30, 0, 0, time.UTC)
	got := CurrentMonth(now)
	if got != (Month{2024, time.March}) {
		t.Errorf("CurrentMonth(%v) = %+v", now, got)
	}
}
