package pull

import (
	"regexp"
	"strings"
)

// Atlassian Document Format (ADF) is the JSON tree Jira returns for rich-text
// fields (issue description, comment bodies). It's a recursive structure of
// typed nodes: block containers (paragraph, heading, list, …) hold a "content"
// array; leaf "text" nodes carry the actual characters and optional "marks"
// (bold, link, …). We don't render it — we flatten it to plain text for cheap
// keyword/scope signals and pull out every URL it references for
// planning-artifact link detection (Confluence pages, Figma, design docs).

// adfResult is the flattened output of an ADF tree.
type adfResult struct {
	Text string
	URLs []string // first-appearance order, de-duplicated
}

// flattenADF walks an ADF document (as decoded from JSON into Go's
// map[string]interface{} / []interface{} shapes) and returns its visible text
// plus every URL it references — link marks, smart-card attrs, and bare URLs
// pasted into the text. A nil or non-object input yields an empty result.
func flattenADF(doc interface{}) adfResult {
	var b strings.Builder
	urls := newURLSet()
	walkADF(doc, &b, urls)
	text := normalizeADFText(b.String())
	urls.addBareURLs(text)
	return adfResult{Text: text, URLs: urls.ordered()}
}

// blockTypes are ADF node types that should end with a line break so their
// contents don't run into the next block when flattened.
var blockTypes = map[string]bool{
	"paragraph":   true,
	"heading":     true,
	"blockquote":  true,
	"bulletList":  true,
	"orderedList": true,
	// listItem is intentionally absent: it wraps a paragraph, which already
	// emits the line break — listing it too would double-space every item.
	"codeBlock":    true,
	"rule":         true,
	"panel":        true,
	"tableRow":     true,
	"taskItem":     true,
	"decisionItem": true,
}

func walkADF(node interface{}, b *strings.Builder, urls *urlSet) {
	switch n := node.(type) {
	case []interface{}:
		for _, child := range n {
			walkADF(child, b, urls)
		}
	case map[string]interface{}:
		switch typ, _ := n["type"].(string); typ {
		case "text":
			if s, ok := n["text"].(string); ok {
				b.WriteString(s)
			}
			collectLinkMarks(n, urls)
		case "hardBreak":
			b.WriteString("\n")
		case "mention":
			b.WriteString(attrString(n, "text"))
		case "emoji":
			if t := attrString(n, "text"); t != "" {
				b.WriteString(t)
			} else {
				b.WriteString(attrString(n, "shortName"))
			}
		case "inlineCard", "blockCard", "embedCard":
			if u := attrString(n, "url"); u != "" {
				b.WriteString(u)
				urls.add(u)
			}
		default:
			// Generic container: recurse into content, then separate blocks.
			walkADF(n["content"], b, urls)
			if blockTypes[typ] {
				b.WriteByte('\n')
			}
		}
	}
}

// collectLinkMarks pulls the href out of any "link" mark on a text node. The
// anchor text itself is already written by the caller; this captures the
// target URL, which usually does not appear in the visible text.
func collectLinkMarks(textNode map[string]interface{}, urls *urlSet) {
	marks, ok := textNode["marks"].([]interface{})
	if !ok {
		return
	}
	for _, m := range marks {
		mark, ok := m.(map[string]interface{})
		if !ok || mark["type"] != "link" {
			continue
		}
		if href := attrString(mark, "href"); href != "" {
			urls.add(href)
		}
	}
}

// attrString reads node["attrs"][key] as a string, "" if absent.
func attrString(node map[string]interface{}, key string) string {
	attrs, ok := node["attrs"].(map[string]interface{})
	if !ok {
		return ""
	}
	s, _ := attrs[key].(string)
	return s
}

// normalizeADFText trims trailing whitespace per line, collapses runs of blank
// lines to a single blank line, and trims the whole string.
func normalizeADFText(s string) string {
	lines := strings.Split(s, "\n")
	out := make([]string, 0, len(lines))
	blank := 0
	for _, ln := range lines {
		ln = strings.TrimRight(ln, " \t")
		if ln == "" {
			if blank++; blank > 1 {
				continue
			}
		} else {
			blank = 0
		}
		out = append(out, ln)
	}
	return strings.TrimSpace(strings.Join(out, "\n"))
}

// bareURLRe matches an http(s) URL up to the first whitespace or bracketing
// character. Trailing sentence punctuation is stripped by urlSet.addBareURLs.
var bareURLRe = regexp.MustCompile(`https?://[^\s<>()\[\]"']+`)

// urlSet is an insertion-ordered, de-duplicated string set for URLs.
type urlSet struct {
	seen  map[string]bool
	order []string
}

func newURLSet() *urlSet { return &urlSet{seen: map[string]bool{}} }

// add inserts an exact URL (link-mark href or card attr) verbatim.
func (u *urlSet) add(s string) {
	if s == "" || u.seen[s] {
		return
	}
	u.seen[s] = true
	u.order = append(u.order, s)
}

// addBareURLs scans flattened text for pasted URLs, stripping trailing
// sentence punctuation the regex may have swept up.
func (u *urlSet) addBareURLs(text string) {
	for _, m := range bareURLRe.FindAllString(text, -1) {
		u.add(strings.TrimRight(m, ".,;:!?"))
	}
}

func (u *urlSet) ordered() []string { return u.order }
