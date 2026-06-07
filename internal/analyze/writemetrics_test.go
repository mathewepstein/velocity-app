package analyze

import (
	"encoding/json"
	"os"
	"testing"

	"github.com/mathewepstein/velocity/internal/config"
)

// TestWriteMetricsStripsDevs locks the contract that the persisted metrics.json
// never carries the per-dev cohort (every live page reads it from
// /api/contributors|dev), while sibling fields survive and the caller's
// in-memory Result is left intact for the clone/scrub and audit paths.
func TestWriteMetricsStripsDevs(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", t.TempDir())

	r := &Result{
		CurrentMonth: "2026-06",
		Current:      WindowMetrics{Label: "current"},
		Devs: []DevWindowMetrics{
			{Dev: config.DevIdentity{DisplayName: "Mathew Epstein"}},
			{Dev: config.DevIdentity{DisplayName: "unknown"}},
		},
	}

	path, err := WriteMetrics(r)
	if err != nil {
		t.Fatalf("WriteMetrics: %v", err)
	}

	// The caller's Result must be untouched (shallow-copy strip, not a mutation).
	if len(r.Devs) != 2 {
		t.Errorf("WriteMetrics mutated caller Devs: len = %d, want 2", len(r.Devs))
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read metrics.json: %v", err)
	}

	// devs must be absent from the persisted blob; sibling fields must remain.
	var generic map[string]json.RawMessage
	if err := json.Unmarshal(raw, &generic); err != nil {
		t.Fatalf("decode metrics.json: %v", err)
	}
	if _, ok := generic["devs"]; ok {
		t.Errorf("persisted metrics.json still contains a `devs` key")
	}
	if _, ok := generic["current_month"]; !ok {
		t.Errorf("persisted metrics.json dropped a sibling field (current_month)")
	}
}
