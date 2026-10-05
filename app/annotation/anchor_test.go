package annotation

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/umputun/revdiff/app/diff"
)

// mkLines builds diff lines from "+text", "-text", " text" and "~" (divider)
// specs, numbering old and new sides the way parseUnifiedDiff does.
func mkLines(specs ...string) []diff.DiffLine {
	var out []diff.DiffLine
	oldNum, newNum := 0, 0
	for _, s := range specs {
		if s == "~" {
			out = append(out, diff.DiffLine{ChangeType: diff.ChangeDivider, Content: "⋯"})
			oldNum += 10
			newNum += 10
			continue
		}
		text := s[1:]
		switch s[0] {
		case '+':
			newNum++
			out = append(out, diff.DiffLine{ChangeType: diff.ChangeAdd, NewNum: newNum, Content: text})
		case '-':
			oldNum++
			out = append(out, diff.DiffLine{ChangeType: diff.ChangeRemove, OldNum: oldNum, Content: text})
		default:
			oldNum++
			newNum++
			out = append(out, diff.DiffLine{ChangeType: diff.ChangeContext, OldNum: oldNum, NewNum: newNum, Content: text})
		}
	}
	return out
}

func TestNewAnchor(t *testing.T) {
	lines := mkLines(" a", " b", "~", "+c", "-d", " e", " f", " g")
	an := NewAnchor(lines, 3)
	require.NotNil(t, an)
	assert.Equal(t, &Anchor{Line: 13, Type: "+", Content: "c", Before: []string{"b", "a"}, After: []string{"d", "e"}}, an)

	assert.Nil(t, NewAnchor(lines, 2), "divider")
	assert.Nil(t, NewAnchor(lines, -1))
	assert.Nil(t, NewAnchor(lines, len(lines)))

	first := NewAnchor(lines, 0)
	assert.Empty(t, first.Before)
	assert.Equal(t, []string{"b", "c"}, first.After)
	assert.Equal(t, 13, NewAnchor(lines, 4).Line, "removed lines use the old-side number")
}

func TestReanchorFile(t *testing.T) {
	before := mkLines(" package a", " ", "+func A() {", "+\treturn compute(1)", "+}", " ", " func B() {}")
	anchored := func(line int, typ string) Annotation {
		idx := -1
		for i, dl := range before {
			if lineNum(dl) == line && string(dl.ChangeType) == typ {
				idx = i
			}
		}
		require.GreaterOrEqual(t, idx, 0)
		return Annotation{File: "a.go", Line: line, Type: typ, Comment: "note", Anchor: NewAnchor(before, idx)}
	}

	tests := []struct {
		name       string
		ann        Annotation
		after      []diff.DiffLine
		wantLine   int
		wantStatus Status
		wantType   string
	}{
		{name: "unchanged stays put", ann: anchored(4, "+"), after: before, wantLine: 4, wantType: "+"},
		{name: "shifted down by inserted lines",
			ann:      anchored(4, "+"),
			after:    mkLines(" package a", " ", "+// doc", "+// more", "+func A() {", "+\treturn compute(1)", "+}", " ", " func B() {}"),
			wantLine: 6, wantType: "+"},
		{name: "edited line is outdated and keeps its line",
			ann:      anchored(4, "+"),
			after:    mkLines(" package a", " ", "+func A() {", "+\treturn compute(2)", "+}", " ", " func B() {}"),
			wantLine: 4, wantStatus: StatusOutdated, wantType: "+"},
		{name: "deleted line is outdated and detached when its line is gone",
			ann:      anchored(4, "+"),
			after:    mkLines(" package a", " ", "+func A() {}"),
			wantLine: -1, wantStatus: StatusOutdated, wantType: "+"},
		{name: "same text with weaker context is still current",
			ann:      anchored(4, "+"),
			after:    mkLines(" package a", " ", "+func Renamed() {", "+\treturn compute(1)", "+\t// trailing", "+}"),
			wantLine: 4, wantType: "+"},
		{name: "moved block is found far away",
			ann: anchored(4, "+"),
			after: mkLines(" package a", " ", " func B() {}", " ", " // filler", " // filler 2", " // filler 3",
				"+func A() {", "+\treturn compute(1)", "+}"),
			wantLine: 9, wantType: "+"},
		{name: "ambiguous text without context is outdated",
			ann:      anchored(4, "+"),
			after:    mkLines(" x", "+\treturn compute(1)", " y", " z", "+\treturn compute(1)", " w"),
			wantLine: -1, wantStatus: StatusOutdated, wantType: "+"},
		{name: "ambiguous text resolved by context",
			ann:      anchored(4, "+"),
			after:    mkLines(" x", "+\treturn compute(1)", " y", "+func A() {", "+\treturn compute(1)", "+}"),
			wantLine: 5, wantType: "+"},
		{name: "trivial line needs context",
			ann:      anchored(5, "+"),
			after:    mkLines(" other", "+}", " more"),
			wantLine: -1, wantStatus: StatusOutdated, wantType: "+"},
		{name: "resolved stays resolved when the line changes",
			ann: func() Annotation {
				a := anchored(4, "+")
				a.Status = StatusResolved
				return a
			}(),
			after:    mkLines(" package a", " ", "+func A() {", "+\treturn compute(2)", "+}"),
			wantLine: 4, wantStatus: StatusResolved, wantType: "+"},
		{name: "outdated comes back when the text returns",
			ann: func() Annotation {
				a := anchored(4, "+")
				a.Status = StatusOutdated
				a.Line = -1
				return a
			}(),
			after: before, wantLine: 4, wantType: "+"},
		{name: "file-level is untouched",
			ann:   Annotation{File: "a.go", Comment: "overall"},
			after: mkLines(" changed"), wantLine: 0},
		{name: "no anchor captures one at the current line",
			ann:   Annotation{File: "a.go", Line: 3, Type: "+", Comment: "x"},
			after: before, wantLine: 3, wantType: "+"},
		{name: "no anchor and no line is kept unverified",
			ann:   Annotation{File: "a.go", Line: 99, Type: "+", Comment: "x"},
			after: before, wantLine: 99, wantType: "+"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, _ := ReanchorFile([]Annotation{tt.ann}, tt.after)
			require.Len(t, got, 1)
			assert.Equal(t, tt.wantLine, got[0].Line)
			assert.Equal(t, tt.wantStatus, got[0].Status)
			assert.Equal(t, tt.wantType, got[0].Type)
			assert.Equal(t, tt.ann.Comment, got[0].Comment)
		})
	}

	t.Run("anchor is captured for annotations without one", func(t *testing.T) {
		got, changed := ReanchorFile([]Annotation{{File: "a.go", Line: 3, Type: "+"}}, before)
		assert.True(t, changed)
		require.NotNil(t, got[0].Anchor)
		assert.Equal(t, "func A() {", got[0].Anchor.Content)
	})

	t.Run("unchanged input reports no change", func(t *testing.T) {
		_, changed := ReanchorFile([]Annotation{anchored(4, "+"), {File: "a.go", Comment: "f"}}, before)
		assert.False(t, changed)
	})

	t.Run("multi-line range moves with its start", func(t *testing.T) {
		a := anchored(3, "+")
		a.EndLine = 5
		got, _ := ReanchorFile([]Annotation{a},
			mkLines(" package a", " ", "+// doc", "+func A() {", "+\treturn compute(1)", "+}", " ", " func B() {}"))
		assert.Equal(t, 4, got[0].Line)
		assert.Equal(t, 6, got[0].EndLine)
	})

	t.Run("removed-line annotation follows old-side numbers", func(t *testing.T) {
		old := mkLines(" a", "-gone soon", "+replacement", " b")
		a := Annotation{File: "a.go", Line: 2, Type: "-", Comment: "why remove", Anchor: NewAnchor(old, 1)}
		got, _ := ReanchorFile([]Annotation{a}, mkLines(" top", " a", "-gone soon", "+replacement", " b"))
		assert.Equal(t, 3, got[0].Line)
		assert.Equal(t, StatusOpen, got[0].Status)
	})

	t.Run("outdated line taken by a current annotation is detached", func(t *testing.T) {
		moved := anchored(4, "+") // will be found at line 4 again
		stale := Annotation{File: "a.go", Line: 4, Type: "+", Comment: "stale",
			Anchor: &Anchor{Line: 4, Type: "+", Content: "no longer here"}}
		got, _ := ReanchorFile([]Annotation{stale, moved}, before)
		require.Len(t, got, 2)
		assert.Equal(t, -1, got[0].Line)
		assert.Equal(t, StatusOutdated, got[0].Status)
		assert.Equal(t, 4, got[1].Line)
		assert.Equal(t, StatusOpen, got[1].Status)
	})

	t.Run("two annotations finding the same line keep one", func(t *testing.T) {
		a1, a2 := anchored(4, "+"), anchored(4, "+")
		a2.Comment = "second"
		got, _ := ReanchorFile([]Annotation{a1, a2}, before)
		assert.Equal(t, StatusOpen, got[0].Status)
		assert.Equal(t, StatusOutdated, got[1].Status)
		assert.Equal(t, -1, got[1].Line)
	})
}

func TestAnnotation_Pending(t *testing.T) {
	assert.True(t, Annotation{}.Pending())
	assert.False(t, Annotation{Delivered: true}.Pending())
	assert.False(t, Annotation{Status: StatusOutdated}.Pending())
	assert.False(t, Annotation{Status: StatusResolved}.Pending())
}
