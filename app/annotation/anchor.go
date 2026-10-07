package annotation

import (
	"slices"
	"strings"

	"github.com/umputun/revdiff/app/diff"
)

// anchorContext is how many neighboring lines an anchor records on each side.
const anchorContext = 2

// Anchor snapshots the diff line an annotation was written on, plus a little
// context, so the annotation can be found again after the code changes.
type Anchor struct {
	Line    int      `json:"line"`             // line number the annotation was anchored at
	Type    string   `json:"type"`             // change type of the anchored line
	Content string   `json:"content"`          // anchored line text
	Before  []string `json:"before,omitempty"` // preceding lines, nearest first
	After   []string `json:"after,omitempty"`  // following lines, nearest first
}

// NewAnchor captures the anchor for lines[idx], recording up to two non-divider
// neighbors on each side. Returns nil for an out-of-range index or a divider.
func NewAnchor(lines []diff.DiffLine, idx int) *Anchor {
	if idx < 0 || idx >= len(lines) || lines[idx].ChangeType == diff.ChangeDivider {
		return nil
	}
	dl := lines[idx]
	return &Anchor{
		Line:    lineNum(dl),
		Type:    string(dl.ChangeType),
		Content: dl.Content,
		Before:  neighbors(lines, idx, -1),
		After:   neighbors(lines, idx, 1),
	}
}

// ReanchorFile re-anchors the annotations of one file against its current
// diff lines and returns the updated set plus whether anything changed.
//
// File-level annotations are kept as they are (the caller decides by whether
// the file is still in the diff). A line annotation whose anchored line is
// found again moves there and is current: an outdated one becomes open again,
// a resolved one stays resolved. One that is not found becomes outdated
// (resolved stays resolved) and keeps its line when that line still exists in
// the diff and no current annotation took it; otherwise it is detached to a
// negative line number, where it renders nowhere but stays listed. An
// annotation without an anchor (e.g. preloaded with --annotations) gets one
// captured at its current line when that line exists, and is otherwise kept
// unchanged — it cannot be verified.
func ReanchorFile(anns []Annotation, lines []diff.DiffLine) ([]Annotation, bool) {
	out := make([]Annotation, 0, len(anns))
	var lost []int // indexes into out of annotations whose anchor was not found
	claimed := make(map[lineKey]bool, len(anns))
	changed := false
	for _, a := range anns {
		updated, current := reanchor(a, lines)
		key := lineKey{updated.Line, updated.Type}
		if current && updated.Line != 0 && claimed[key] {
			// two annotations found the same line; the first keeps it
			current = false
			if updated.Status != StatusResolved {
				updated.Status = StatusOutdated
			}
		}
		if !annotationsEqual(a, updated) {
			changed = true
		}
		out = append(out, updated)
		if updated.Line == 0 {
			continue
		}
		if current {
			claimed[key] = true
			continue
		}
		lost = append(lost, len(out)-1)
	}

	present := make(map[lineKey]bool, len(lines))
	for _, dl := range lines {
		if dl.ChangeType != diff.ChangeDivider {
			present[lineKey{lineNum(dl), string(dl.ChangeType)}] = true
		}
	}
	detached := 0
	for _, i := range lost {
		line, typ := out[i].Line, out[i].Type
		if out[i].Anchor != nil {
			line, typ = out[i].Anchor.Line, out[i].Anchor.Type
		}
		key := lineKey{line, typ}
		if line > 0 && present[key] && !claimed[key] {
			claimed[key] = true
		} else {
			detached--
			line = detached
		}
		if out[i].Line != line || out[i].Type != typ {
			out[i].Line, out[i].Type = line, typ
			changed = true
		}
	}
	return out, changed
}

type lineKey struct {
	line int
	typ  string
}

// reanchor locates a line annotation in lines. current is false when its
// anchored line no longer exists.
func reanchor(a Annotation, lines []diff.DiffLine) (_ Annotation, current bool) {
	if a.Line == 0 {
		return a, true
	}
	if a.Anchor == nil {
		for i, dl := range lines {
			if string(dl.ChangeType) == a.Type && lineNum(dl) == a.Line {
				a.Anchor = NewAnchor(lines, i)
				return a, true
			}
		}
		return a, true
	}
	idx := a.Anchor.Locate(lines)
	if idx < 0 {
		if a.Status != StatusResolved {
			a.Status = StatusOutdated
		}
		return a, false
	}
	newLine := lineNum(lines[idx])
	if a.EndLine > 0 {
		a.EndLine += newLine - a.Anchor.Line
	}
	a.Line = newLine
	a.Type = string(lines[idx].ChangeType)
	a.Anchor = NewAnchor(lines, idx)
	if a.Status == StatusOutdated {
		a.Status = StatusOpen
	}
	return a, true
}

// Locate returns the index of the line the anchor matches best, or -1.
// Candidates must have the anchored type and text; the best is the one whose
// neighbors match the recorded context most, then the one nearest the old line
// number. A match is refused when it relies on no context at all while being
// ambiguous (several candidates) or trivial (a brace or blank line), since
// such a line says nothing about where the annotation belongs.
func (an *Anchor) Locate(lines []diff.DiffLine) int {
	best, bestScore, bestDist, candidates := -1, -1, 0, 0
	for i, dl := range lines {
		if string(dl.ChangeType) != an.Type || dl.Content != an.Content {
			continue
		}
		candidates++
		score := an.contextScore(lines, i)
		dist := lineNum(dl) - an.Line
		if dist < 0 {
			dist = -dist
		}
		if score > bestScore || (score == bestScore && dist < bestDist) {
			best, bestScore, bestDist = i, score, dist
		}
	}
	if best < 0 {
		return -1
	}
	if bestScore == 0 && (candidates > 1 || an.trivial()) {
		return -1
	}
	return best
}

// contextScore counts recorded neighbors that match the neighbors of lines[idx].
func (an *Anchor) contextScore(lines []diff.DiffLine, idx int) int {
	score := 0
	for k, got := range neighbors(lines, idx, -1) {
		if k < len(an.Before) && an.Before[k] == got {
			score++
		}
	}
	for k, got := range neighbors(lines, idx, 1) {
		if k < len(an.After) && an.After[k] == got {
			score++
		}
	}
	return score
}

// trivial reports whether the anchored text has too little content to identify
// a line on its own (blank lines, lone braces or brackets).
func (an *Anchor) trivial() bool {
	return len(strings.TrimSpace(an.Content)) <= 2
}

// neighbors returns the text of up to anchorContext non-divider lines next to
// lines[idx] in direction step (-1 before, +1 after), nearest first.
func neighbors(lines []diff.DiffLine, idx, step int) []string {
	var out []string
	for i := idx + step; i >= 0 && i < len(lines) && len(out) < anchorContext; i += step {
		if lines[i].ChangeType == diff.ChangeDivider {
			continue
		}
		out = append(out, lines[i].Content)
	}
	return out
}

// lineNum is the display line number of a diff line: the old-side number for
// removed lines, the new-side number otherwise.
func lineNum(dl diff.DiffLine) int {
	if dl.ChangeType == diff.ChangeRemove {
		return dl.OldNum
	}
	return dl.NewNum
}

// annotationsEqual compares two annotations including their anchors' values.
func annotationsEqual(a, b Annotation) bool {
	aa, ba := a.Anchor, b.Anchor
	a.Anchor, b.Anchor = nil, nil
	if a != b {
		return false
	}
	if aa == nil || ba == nil {
		return aa == ba
	}
	return aa.Line == ba.Line && aa.Type == ba.Type && aa.Content == ba.Content &&
		slices.Equal(aa.Before, ba.Before) && slices.Equal(aa.After, ba.After)
}
