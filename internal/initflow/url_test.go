package initflow

import "testing"

func TestNormalizeJiraURL(t *testing.T) {
	tests := []struct {
		in      string
		want    string
		wantErr bool
	}{
		{"consumerdirect", "https://consumerdirect.atlassian.net", false},
		{"consumerdirect.atlassian.net", "https://consumerdirect.atlassian.net", false},
		{"https://consumerdirect.atlassian.net", "https://consumerdirect.atlassian.net", false},
		{"https://consumerdirect.atlassian.net/", "https://consumerdirect.atlassian.net", false},
		{"http://localhost:8080", "http://localhost:8080", false},
		{"  consumerdirect  ", "https://consumerdirect.atlassian.net", false},
		{"", "", true},
		{"ftp://example.com", "", true},
	}
	for _, tc := range tests {
		got, err := NormalizeJiraURL(tc.in)
		if tc.wantErr {
			if err == nil {
				t.Errorf("NormalizeJiraURL(%q): expected error, got %q", tc.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("NormalizeJiraURL(%q): unexpected error: %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("NormalizeJiraURL(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
