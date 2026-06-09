package jirafields

import (
	"fmt"
	"sort"
	"strings"
)

// FieldMeta is the catalog entry for one Jira field (from /rest/api/3/field).
type FieldMeta struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Custom bool   `json:"custom"`
}

// FieldStat is how often a field was populated across the sampled issues, plus
// a one-line shape descriptor of a sample value — the evidence the operator
// reviews before deciding what to map or capture.
type FieldStat struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Custom    bool   `json:"custom"`
	Populated int    `json:"populated"` // # sampled issues where the field had a value
	Sampled   int    `json:"sampled"`   // # issues examined
	Shape     string `json:"shape"`     // descriptor of a populated sample value
}

// Proposal is a suggested canonical→field-id mapping for a signal that has a
// consumer in the engine (story_points, epic_link, description). Standard
// fields and consumer-less custom fields are never proposed here.
type Proposal struct {
	Canonical string `json:"canonical"`
	FieldID   string `json:"field_id"`
	FieldName string `json:"field_name"`
	Reason    string `json:"reason"`
}

// Report is the wizard's read-only output: per-field population evidence,
// proposed canonical mappings, and the rest of the populated custom fields
// split into capture-worthy suggestions vs. denylisted noise.
type Report struct {
	TicketsScanned int        `json:"tickets_scanned"`
	Keys           []string   `json:"keys"`
	Proposed       []Proposal `json:"proposed"`   // named mappings (have consumers)
	Extra          []FieldStat `json:"extra"`      // populated custom fields worth capturing, no named mapping
	Denylisted     []FieldStat `json:"denylisted"` // populated custom fields excluded as noise
	Stats          []FieldStat `json:"stats"`      // every field seen, populated-desc (full evidence)
}

// noiseKeywords are lowercased substrings that mark a field as JSM / HR /
// finance / SLA noise rather than an engineering-effort signal. Matched against
// the field name (org-agnostic — field IDs vary per instance). The wizard only
// *proposes*; the operator curates, so this errs toward flagging the obvious
// service-desk and people-ops fields without trying to be exhaustive.
var noiseKeywords = []string{
	"request type", "approval", "approver", "organizations",
	"satisfaction", "sla", "time to first response", "time to resolution",
	"time to done", "responded", "first response", "sentiment",
	"manager", "job title", "employment", "working day", "resignation",
	"onboard", "offboard", "budget", "deal id", "amount", "azure environment",
	"marketing", "contract", "request participants", "request language",
}

// buildReport is the pure core: given the field catalog and the raw `fields`
// maps of the sampled issues, it tallies population, proposes the consumer-
// backed mappings, and buckets the remaining populated custom fields. No I/O —
// directly unit-testable.
func buildReport(catalog []FieldMeta, keys []string, issues []map[string]interface{}) *Report {
	byID := make(map[string]FieldMeta, len(catalog))
	for _, m := range catalog {
		byID[m.ID] = m
	}

	sampled := len(issues)
	popCount := map[string]int{}
	shape := map[string]string{}
	for _, f := range issues {
		for id, v := range f {
			if !isPopulated(v) {
				continue
			}
			popCount[id]++
			if shape[id] == "" {
				shape[id] = shapeOf(v)
			}
		}
	}

	// Every field that was populated at least once becomes a stat. Fields in the
	// catalog but never populated in the sample are omitted from Stats (the
	// operator cares about what's actually used), but still resolvable by ID.
	stats := make([]FieldStat, 0, len(popCount))
	for id, n := range popCount {
		meta := byID[id]
		name := meta.Name
		if name == "" {
			name = id // field not in catalog (shouldn't happen, but don't drop it)
		}
		stats = append(stats, FieldStat{
			ID:        id,
			Name:      name,
			Custom:    meta.Custom,
			Populated: n,
			Sampled:   sampled,
			Shape:     shape[id],
		})
	}
	sortStats(stats)

	rep := &Report{
		TicketsScanned: sampled,
		Keys:           keys,
		Stats:          stats,
	}
	rep.Proposed = proposeMappings(catalog, stats)

	// Bucket the remaining populated custom fields (excluding any chosen as a
	// proposal) into capture-worthy vs. denylisted noise.
	chosen := map[string]bool{}
	for _, p := range rep.Proposed {
		chosen[p.FieldID] = true
	}
	for _, s := range stats {
		if !s.Custom || chosen[s.ID] {
			continue
		}
		if isNoise(s.Name) {
			rep.Denylisted = append(rep.Denylisted, s)
		} else {
			rep.Extra = append(rep.Extra, s)
		}
	}
	return rep
}

// proposeMappings picks the consumer-backed canonical mappings. Only
// story_points, epic_link, and description have engine consumers today; nothing
// else is proposed as a named mapping.
//
// story_points and epic_link are matched against the field CATALOG by name —
// these fields are routinely empty on recent/open tickets, so a population-only
// match would miss them in a small sample. description is matched by population
// among description-named fields, because that is exactly the signal that
// distinguishes a sparse standard `description` from the custom field actually
// in use.
func proposeMappings(catalog []FieldMeta, stats []FieldStat) []Proposal {
	var out []Proposal
	pop := map[string]int{}
	sampled := 0
	for _, s := range stats {
		pop[s.ID] = s.Populated
		sampled = s.Sampled
	}

	// story_points: catalog field named "story points" / "story point estimate".
	if m, ok := catalogByName(catalog, func(n string) bool {
		return n == "story points" || n == "story point estimate"
	}); ok {
		out = append(out, Proposal{
			Canonical: "story_points", FieldID: m.ID, FieldName: m.Name,
			Reason: fmt.Sprintf("catalog field %q, populated on %d/%d sampled", m.Name, pop[m.ID], sampled),
		})
	}

	// epic_link: a custom "epic link" field if one exists, else the built-in
	// "parent" (always valid on company-managed projects).
	if m, ok := catalogByName(catalog, func(n string) bool { return n == "epic link" }); ok {
		out = append(out, Proposal{
			Canonical: "epic_link", FieldID: m.ID, FieldName: m.Name,
			Reason: fmt.Sprintf("catalog field %q, populated on %d/%d sampled", m.Name, pop[m.ID], sampled),
		})
	} else {
		out = append(out, Proposal{
			Canonical: "epic_link", FieldID: "parent", FieldName: "Parent",
			Reason: "no custom \"Epic Link\" field found; using built-in parent",
		})
	}

	// description: among fields whose name contains "description", take the most
	// populated (stats are population-desc). Falls back to a catalog match if
	// none were populated in the sample.
	if s, ok := pickMostPopulated(stats, func(n string) bool {
		return strings.Contains(n, "description")
	}); ok {
		reason := fmt.Sprintf("most-populated description-named field (%d/%d sampled)", s.Populated, s.Sampled)
		if s.ID == "description" {
			reason = fmt.Sprintf("standard description field, populated on %d/%d sampled", s.Populated, s.Sampled)
		}
		out = append(out, Proposal{
			Canonical: "description", FieldID: s.ID, FieldName: s.Name, Reason: reason,
		})
	} else if m, ok := catalogByName(catalog, func(n string) bool { return strings.Contains(n, "description") }); ok {
		out = append(out, Proposal{
			Canonical: "description", FieldID: m.ID, FieldName: m.Name,
			Reason: "description-named catalog field (unpopulated in sample — verify)",
		})
	}
	return out
}

// catalogByName returns the first catalog field whose lowercased, trimmed name
// satisfies match. ok is false when none match.
func catalogByName(catalog []FieldMeta, match func(name string) bool) (FieldMeta, bool) {
	for _, m := range catalog {
		if match(strings.ToLower(strings.TrimSpace(m.Name))) {
			return m, true
		}
	}
	return FieldMeta{}, false
}

// pickMostPopulated returns the highest-population field whose lowercased name
// satisfies match (stats are pre-sorted population-desc, so the first hit wins).
func pickMostPopulated(stats []FieldStat, match func(name string) bool) (FieldStat, bool) {
	for _, s := range stats {
		if match(strings.ToLower(strings.TrimSpace(s.Name))) {
			return s, true
		}
	}
	return FieldStat{}, false
}

func isNoise(name string) bool {
	n := strings.ToLower(strings.TrimSpace(name))
	for _, kw := range noiseKeywords {
		if strings.Contains(n, kw) {
			return true
		}
	}
	return false
}

// sortStats orders by population desc, then name asc for stable output.
func sortStats(stats []FieldStat) {
	sort.SliceStable(stats, func(i, j int) bool {
		if stats[i].Populated != stats[j].Populated {
			return stats[i].Populated > stats[j].Populated
		}
		return stats[i].Name < stats[j].Name
	})
}

// isPopulated reports whether a raw Jira field value carries real content.
// Conservative: empty strings, empty arrays/objects, null, false, and numeric
// zero all count as unpopulated so the population tally reflects actual usage.
func isPopulated(v interface{}) bool {
	switch t := v.(type) {
	case nil:
		return false
	case string:
		return strings.TrimSpace(t) != ""
	case bool:
		return t
	case float64:
		return t != 0
	case []interface{}:
		return len(t) > 0
	case map[string]interface{}:
		return len(t) > 0
	default:
		return true
	}
}

// shapeOf renders a one-line descriptor of a populated value so the operator
// can tell at a glance what a field holds.
func shapeOf(v interface{}) string {
	switch t := v.(type) {
	case string:
		return "string"
	case bool:
		return "bool"
	case float64:
		return "number"
	case []interface{}:
		return fmt.Sprintf("array[%d]", len(t))
	case map[string]interface{}:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		if len(keys) > 6 {
			keys = append(keys[:6], "…")
		}
		return "object{" + strings.Join(keys, ",") + "}"
	default:
		return fmt.Sprintf("%T", v)
	}
}
