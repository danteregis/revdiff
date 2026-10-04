package annotation

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestKinds(t *testing.T) {
	assert.Equal(t, []string{"bug", "suggestion", "question", "nitpick", "praise"}, Kinds())

	got := Kinds()
	got[0] = "mutated"
	assert.Equal(t, "bug", Kinds()[0], "Kinds must return a copy")
}

func TestStore_LabelBody(t *testing.T) {
	s := NewStore()
	assert.Equal(t, "text", s.labelBody("", "text"))
	assert.Empty(t, s.labelBody("", ""))
	assert.Equal(t, "praise", s.labelBody("praise", ""))
	assert.Equal(t, "bug: off by one", s.labelBody("bug", "off by one"))
	assert.Equal(t, "question: why?\nsecond", s.labelBody("question", "why?\nsecond"))
}

func TestParser_SplitLabel(t *testing.T) {
	tests := []struct {
		name, body, wantKind, wantComment string
	}{
		{name: "untyped", body: "plain text", wantComment: "plain text"},
		{name: "empty", body: ""},
		{name: "bare label", body: "praise", wantKind: "praise"},
		{name: "label with space", body: "bug: off by one", wantKind: "bug", wantComment: "off by one"},
		{name: "label without space", body: "nitpick:rename", wantKind: "nitpick", wantComment: "rename"},
		{name: "only one space stripped", body: "bug:  two", wantKind: "bug", wantComment: " two"},
		{name: "multi-line", body: "question: why?\nsecond\nthird", wantKind: "question", wantComment: "why?\nsecond\nthird"},
		{name: "bare label then lines", body: "suggestion\nrest", wantKind: "suggestion", wantComment: "rest"},
		{name: "unknown label kept", body: "issue: broken", wantComment: "issue: broken"},
		{name: "label prefix of a word", body: "bugfix: later", wantComment: "bugfix: later"},
		{name: "case sensitive", body: "Bug: loud", wantComment: "Bug: loud"},
		{name: "label on second line ignored", body: "first\nbug: second", wantComment: "first\nbug: second"},
	}
	p := &parser{}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			kind, comment := p.splitLabel(tt.body)
			assert.Equal(t, tt.wantKind, kind)
			assert.Equal(t, tt.wantComment, comment)
		})
	}
}

func TestStore_FormatOutputKinds(t *testing.T) {
	s := NewStore()
	s.Add(Annotation{File: "a.go", Line: 0, Kind: "suggestion", Comment: "split this file"})
	s.Add(Annotation{File: "a.go", Line: 3, Type: "+", Kind: "praise"})
	s.Add(Annotation{File: "a.go", Line: 5, Type: "-", Kind: "bug", Comment: "off by one\nsee loop"})
	s.Add(Annotation{File: "a.go", Line: 7, Type: " ", Comment: "untyped"})

	want := "## a.go (file-level)\nsuggestion: split this file\n\n" +
		"## a.go:3 (+)\npraise\n\n" +
		"## a.go:5 (-)\nbug: off by one\nsee loop\n\n" +
		"## a.go:7 ( )\nuntyped\n"
	assert.Equal(t, want, s.FormatOutput())
}

func TestStore_AddReplacesKind(t *testing.T) {
	s := NewStore()
	s.Add(Annotation{File: "a.go", Line: 1, Type: "+", Comment: "x", Kind: "bug"})
	s.Add(Annotation{File: "a.go", Line: 1, Type: "+", Comment: "x", Kind: "praise"})
	got := s.Get("a.go")
	require.Len(t, got, 1)
	assert.Equal(t, "praise", got[0].Kind)

	s.Add(Annotation{File: "a.go", Line: 1, Type: "+", Comment: "y"})
	got = s.Get("a.go")
	require.Len(t, got, 1)
	assert.Empty(t, got[0].Kind, "replacement with an untyped annotation clears the kind")
	assert.Equal(t, "y", got[0].Comment)
}

func TestKinds_RoundTrip(t *testing.T) {
	in := []Annotation{
		{File: "a.go", Line: 0, Kind: "praise"},
		{File: "a.go", Line: 2, Type: "+", Kind: "praise"},
		{File: "a.go", Line: 4, Type: "+", Kind: "bug", Comment: "off by one"},
		{File: "a.go", Line: 6, Type: "-", Kind: "question", Comment: "why?\nsecond line\n\nfourth"},
		{File: "a.go", Line: 8, Type: " ", Kind: "nitpick", Comment: "## not a header\n## also not"},
		{File: "a.go", Line: 10, EndLine: 14, Type: "+", Kind: "suggestion", Comment: "rework this hunk"},
		{File: "a.go", Line: 16, Type: "+", Kind: "bug", Comment: "\nleading blank line"},
		{File: "a.go", Line: 18, Type: "+", Comment: "plain\nmulti"},
		{File: "a.go", Line: 20, Type: "+", Comment: ""},
		{File: "b.go", Line: 0, Kind: "suggestion", Comment: "file-level\nmulti"},
		{File: "b.go", Line: 1, Type: "+", Comment: "issue: unknown label stays text"},
	}
	s := NewStore()
	for _, a := range in {
		s.Add(a)
	}
	out := s.FormatOutput()

	got, err := Parse(strings.NewReader(out))
	require.NoError(t, err)
	assert.Equal(t, in, got)

	// a second pass over the reparsed store must be byte-identical
	s2 := NewStore()
	for _, a := range got {
		s2.Add(a)
	}
	assert.Equal(t, out, s2.FormatOutput())
}
