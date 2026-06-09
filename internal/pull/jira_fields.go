package pull

import (
	"encoding/json"
	"sort"

	"github.com/mathewepstein/velocity/internal/cache"
	"github.com/mathewepstein/velocity/internal/jirafields"
)

// descriptionText flattens the ADF (or plain) value at fields[key] to text,
// returning "" when absent/empty. The comprehensive hydration reads the mapped
// description field with this, falling back to the standard "description".
func descriptionText(fields map[string]interface{}, key string) string {
	v, ok := fields[key]
	if !ok || v == nil {
		return ""
	}
	return flattenADF(v).Text
}

// buildRawFields encodes every populated field (minus the noise denylist) into
// the catch-all, keyed by ID with its human name and JSON value — the "never
// crawl Jira again" store, so a future signal derives from it without a re-pull.
// Deterministic (ID order) so a re-pull of the same issue produces byte-
// identical rows. The denylist + populated check are shared with the discover
// wizard (jirafields) so capture and proposal stay in lockstep.
func buildRawFields(fields map[string]interface{}, names map[string]string) []cache.RawField {
	ids := make([]string, 0, len(fields))
	for id := range fields {
		ids = append(ids, id)
	}
	sort.Strings(ids)

	out := []cache.RawField{}
	for _, id := range ids {
		v := fields[id]
		if !jirafields.IsPopulated(v) {
			continue
		}
		name := names[id]
		if jirafields.IsNoiseFieldName(name) {
			continue
		}
		b, err := json.Marshal(v)
		if err != nil {
			continue
		}
		out = append(out, cache.RawField{ID: id, Name: name, Value: string(b)})
	}
	return out
}
