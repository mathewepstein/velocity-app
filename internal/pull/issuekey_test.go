package pull

import (
	"reflect"
	"testing"
)

func TestExtractIssueKeys(t *testing.T) {
	tests := []struct {
		name  string
		texts []string
		want  []string
	}{
		{"single in title", []string{"CD-123 fix the thing"}, []string{"CD-123"}},
		{"branch and body", []string{"CD-22805-refetch", "Fixes CD-22805 and also CD-10000"}, []string{"CD-22805", "CD-10000"}},
		{"dedupe preserves first-seen order", []string{"CD-1 CD-2 CD-1 CD-3"}, []string{"CD-1", "CD-2", "CD-3"}},
		{"no match in lowercase", []string{"cd-123 xyz-9"}, nil},
		{"word boundary prevents false match", []string{"ABCD-5-extra-is-fine, but NOT MATCH-1Zfoo"}, []string{"ABCD-5"}},
		{"empty input", []string{"", "   "}, nil},
		{"mixed project prefixes", []string{"WAPI-100 and PAPI-200"}, []string{"WAPI-100", "PAPI-200"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := ExtractIssueKeys(tc.texts...)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}
