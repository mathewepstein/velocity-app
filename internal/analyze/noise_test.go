package analyze

import (
	"testing"

	"github.com/mathewepstein/velocity/internal/config"
)

func TestGenFileWeight_DampensNoisePaths(t *testing.T) {
	cfg := config.NormalizeConfig{
		GeneratedFileWeight:   0.25,
		GeneratedFilePatterns: []string{"*.lock"},
		NoisePaths:            []string{"**/src/stories/**", "**/.storybook/**"},
	}
	cases := []struct {
		path string
		want float64
	}{
		{"src/stories/Button.stories.ts", 0.25}, // noise → dampened
		{".storybook/main.ts", 0.25},            // noise → dampened
		{"yarn.lock", 0.25},                     // generated → dampened
		{"src/components/Button.vue", 1.0},      // real code → full weight
	}
	for _, c := range cases {
		if got := genFileWeight(c.path, cfg); got != c.want {
			t.Errorf("genFileWeight(%q) = %v, want %v", c.path, got, c.want)
		}
	}
}

func TestMatchesNoisePath(t *testing.T) {
	cfg := config.NormalizeConfig{NoisePaths: []string{"**/src/stories/**", "**/e2e/test-results/**"}}
	if !MatchesNoisePath("frontend/app/src/stories/Foo.stories.ts", cfg) {
		t.Error("stories path should match")
	}
	if !MatchesNoisePath("e2e/test-results/run-1/trace.zip", cfg) {
		t.Error("test-results path should match")
	}
	if MatchesNoisePath("src/components/Foo.vue", cfg) {
		t.Error("real code should not match")
	}
	// Empty config matches nothing.
	if MatchesNoisePath("anything", config.NormalizeConfig{}) {
		t.Error("empty noise config should match nothing")
	}
}
