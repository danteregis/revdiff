package notes

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/umputun/revdiff/app/annotation"
	"github.com/umputun/revdiff/app/diff"
)

const sampleDoc = `{"format":"revdiff-notes/v1","target":{"ref":"origin/master...feat/x","head":"abc"},"author":"claude",
"tour":["b.go","a.go","b.go"],
"files":[
 {"path":"a.go","overview":"what a does","notes":[{"kind":"caution","line":2,"body":"careful"},{"id":"n7","line":3,"side":"-","body":"gone"}]},
 {"path":"b.go","notes":[{"line":1,"body":"entry point"}]}
]}`

func TestParse(t *testing.T) {
	doc, err := Parse([]byte(sampleDoc))
	require.NoError(t, err)
	assert.Equal(t, "origin/master...feat/x", doc.Target.Ref)
	assert.Equal(t, []string{"b.go", "a.go"}, doc.Tour, "tour is deduplicated")
	require.Len(t, doc.Files, 2)

	a := doc.Files[0]
	assert.Empty(t, a.Overview, "overview shorthand becomes a note")
	require.Len(t, a.Notes, 3)
	assert.Equal(t, KindOverview, a.Notes[0].Kind)
	assert.Equal(t, "what a does", a.Notes[0].Body)
	assert.Equal(t, KindCaution, a.Notes[1].Kind)
	assert.Equal(t, SideNew, a.Notes[1].Side, "side defaults to +")
	assert.Equal(t, "n7", a.Notes[2].ID, "given ids are kept")
	assert.Equal(t, KindExplain, doc.Files[1].Notes[0].Kind, "kind defaults to explain")

	ids := map[string]bool{}
	for _, f := range doc.Files {
		for _, n := range f.Notes {
			require.NotEmpty(t, n.ID)
			assert.False(t, ids[n.ID], "duplicate id %s", n.ID)
			ids[n.ID] = true
		}
	}
	assert.Equal(t, 4, doc.Count())
}

func TestParse_Errors(t *testing.T) {
	tests := []struct {
		name, doc, want string
	}{
		{"bad json", `{`, "decode notes"},
		{"wrong format", `{"format":"revdiff-notes/v2","files":[]}`, "format must be"},
		{"unknown field", `{"format":"revdiff-notes/v1","files":[],"extra":1}`, "unknown field"},
		{"trailing", `{"format":"revdiff-notes/v1","files":[]} {}`, "trailing data"},
		{"no path", `{"format":"revdiff-notes/v1","files":[{"path":" "}]}`, "has no path"},
		{"dup path", `{"format":"revdiff-notes/v1","files":[{"path":"a"},{"path":"a"}]}`, "listed twice"},
		{"empty body", `{"format":"revdiff-notes/v1","files":[{"path":"a","notes":[{"line":1,"body":" "}]}]}`, "body is empty"},
		{"bad kind", `{"format":"revdiff-notes/v1","files":[{"path":"a","notes":[{"kind":"todo","line":1,"body":"x"}]}]}`, "unknown kind"},
		{"no line", `{"format":"revdiff-notes/v1","files":[{"path":"a","notes":[{"body":"x"}]}]}`, "line must be positive"},
		{"bad side", `{"format":"revdiff-notes/v1","files":[{"path":"a","notes":[{"line":1,"side":"x","body":"x"}]}]}`, "side must be"},
		{"end before line", `{"format":"revdiff-notes/v1","files":[{"path":"a","notes":[{"line":5,"end_line":2,"body":"x"}]}]}`, "end_line 2"},
		{"bad status", `{"format":"revdiff-notes/v1","files":[{"path":"a","notes":[{"line":1,"status":"x","body":"x"}]}]}`, "unknown status"},
		{"dup id", `{"format":"revdiff-notes/v1","files":[{"path":"a","notes":[{"id":"x","line":1,"body":"x"},{"id":"x","line":2,"body":"y"}]}]}`, "duplicate note id"},
		{"two overviews", `{"format":"revdiff-notes/v1","files":[{"path":"a","overview":"o","notes":[{"kind":"overview","body":"p"}]}]}`, "2 overviews"},
		{"empty tour", `{"format":"revdiff-notes/v1","tour":[""],"files":[]}`, "tour has an empty path"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(tt.doc))
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.want)
		})
	}
}

func TestParse_OverviewDropsPosition(t *testing.T) {
	doc, err := Parse([]byte(`{"format":"revdiff-notes/v1","files":[{"path":"a","notes":[{"kind":"overview","line":4,"side":"-","body":"o"}]}]}`))
	require.NoError(t, err)
	n := doc.Files[0].Notes[0]
	assert.Zero(t, n.Line)
	assert.Empty(t, n.Side)
}

func testLines() []diff.DiffLine {
	return []diff.DiffLine{
		{OldNum: 1, NewNum: 1, Content: "package a", ChangeType: diff.ChangeContext},
		{OldNum: 2, Content: "func Old() {}", ChangeType: diff.ChangeRemove},
		{NewNum: 2, Content: "func New() {}", ChangeType: diff.ChangeAdd},
		{OldNum: 3, NewNum: 3, Content: "var x = 1", ChangeType: diff.ChangeContext},
	}
}

func TestNote_Locate(t *testing.T) {
	lines := testLines()
	tests := []struct {
		name string
		note Note
		want int
	}{
		{"added line", Note{Kind: KindExplain, Line: 2, Side: SideNew}, 2},
		{"removed line", Note{Kind: KindExplain, Line: 2, Side: SideOld}, 1},
		{"context by +", Note{Kind: KindExplain, Line: 3, Side: SideNew}, 3},
		{"context by space", Note{Kind: KindExplain, Line: 1, Side: SideContext}, 0},
		{"space does not match add", Note{Kind: KindExplain, Line: 2, Side: SideContext}, -1},
		{"missing line", Note{Kind: KindExplain, Line: 9, Side: SideNew}, -1},
		{"overview", Note{Kind: KindOverview}, -1},
		{"anchor moves", Note{Kind: KindExplain, Line: 7, Side: SideNew, Anchor: annotation.NewAnchor(lines, 2)}, 2},
		{"anchor text differs at line", Note{Kind: KindExplain, Line: 2, Side: SideNew,
			Anchor: &annotation.Anchor{Line: 2, Type: "+", Content: "func Gone() {}"}}, -1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			assert.Equal(t, tt.want, tt.note.Locate(lines))
		})
	}
}

func TestDocument_AnchorFile(t *testing.T) {
	doc, err := Parse([]byte(`{"format":"revdiff-notes/v1","files":[{"path":"a.go","overview":"o","notes":[
		{"line":2,"end_line":3,"body":"new func"},{"line":2,"side":"-","body":"old func"},{"line":9,"body":"nowhere"}]}]}`))
	require.NoError(t, err)

	assert.Equal(t, 1, doc.AnchorFile("a.go", testLines()))
	notes := doc.FileNotes("a.go")
	require.NotNil(t, notes[1].Anchor)
	assert.Equal(t, "func New() {}", notes[1].Anchor.Content)
	assert.Equal(t, StatusCurrent, notes[1].Status)
	assert.Equal(t, SideOld, notes[2].Side)
	assert.Equal(t, StatusOutdated, notes[3].Status)
	assert.Nil(t, notes[0].Anchor, "overview is never anchored")

	// the code moved down by two lines: the anchored notes follow it
	moved := append([]diff.DiffLine{
		{OldNum: 1, NewNum: 1, Content: "// header", ChangeType: diff.ChangeContext},
		{OldNum: 2, NewNum: 2, Content: "", ChangeType: diff.ChangeContext},
	}, shift(testLines(), 2)...)
	assert.Equal(t, 1, doc.AnchorFile("a.go", moved))
	notes = doc.FileNotes("a.go")
	assert.Equal(t, 4, notes[1].Line)
	assert.Equal(t, 5, notes[1].EndLine, "the range moves with the note")
	assert.Equal(t, 4, notes[2].Line)

	// the file left the diff: every line note is outdated
	assert.Equal(t, 3, doc.AnchorFile("a.go", nil))
	assert.Equal(t, 0, doc.AnchorFile("missing.go", testLines()))
}

func shift(lines []diff.DiffLine, by int) []diff.DiffLine {
	out := make([]diff.DiffLine, len(lines))
	for i, dl := range lines {
		if dl.OldNum > 0 {
			dl.OldNum += by
		}
		if dl.NewNum > 0 {
			dl.NewNum += by
		}
		out[i] = dl
	}
	return out
}

func TestDocument_TourOrder(t *testing.T) {
	doc, err := Parse([]byte(`{"format":"revdiff-notes/v1","tour":["c.go","b.go","zz.go"],"files":[
		{"path":"a.go","overview":"a"},{"path":"b.go","overview":"b"},{"path":"c.go","overview":"c"},{"path":"d.go"}]}`))
	require.NoError(t, err)
	assert.Equal(t, []string{"c.go", "b.go", "a.go"}, doc.TourOrder())
	var nilDoc *Document
	assert.Nil(t, nilDoc.TourOrder())
	assert.Nil(t, nilDoc.FileNotes("a.go"))
	assert.Zero(t, nilDoc.Count())
	assert.Zero(t, nilDoc.PendingReplies())
	n, _ := nilDoc.Find("n1")
	assert.Nil(t, n)
}

func TestDocument_Replace(t *testing.T) {
	cur, err := Parse([]byte(`{"format":"revdiff-notes/v1","files":[{"path":"a.go","notes":[{"id":"n1","line":1,"body":"x"},{"id":"n2","line":2,"body":"y"}]}]}`))
	require.NoError(t, err)
	_, err = cur.Answer("n1", "hello", time.Unix(1, 0))
	require.NoError(t, err)

	incoming, err := Parse([]byte(`{"format":"revdiff-notes/v1","tour":["a.go"],"files":[{"path":"a.go","notes":[{"id":"n1","line":4,"body":"moved"},{"line":5,"body":"new"}]}]}`))
	require.NoError(t, err)
	cur.Replace(incoming)
	assert.Equal(t, []string{"a.go"}, cur.Tour)
	notes := cur.FileNotes("a.go")
	require.Len(t, notes, 2)
	assert.Equal(t, "moved", notes[0].Body)
	require.Len(t, notes[0].Thread, 1, "kept id keeps its thread")
	assert.Equal(t, "hello", notes[0].Thread[0].Body)
	assert.Empty(t, notes[1].Thread)
	assert.NotEqual(t, "n1", notes[1].ID)
}

func TestDocument_AddNote(t *testing.T) {
	doc := &Document{Format: Format}
	n, err := doc.AddNote("a.go", Note{Kind: KindCaution, Line: 3, Body: "watch out", ID: "ignored", Status: StatusOutdated})
	require.NoError(t, err)
	assert.Equal(t, "n1", n.ID)
	assert.Equal(t, StatusCurrent, n.Status)

	ov, err := doc.AddNote("a.go", Note{Kind: KindOverview, Body: "first"})
	require.NoError(t, err)
	assert.Equal(t, KindOverview, doc.FileNotes("a.go")[0].Kind, "overview goes first")
	_, err = doc.Answer(ov.ID, "thread", time.Unix(1, 0))
	require.NoError(t, err)
	again, err := doc.AddNote("a.go", Note{Kind: KindOverview, Body: "second"})
	require.NoError(t, err)
	assert.Equal(t, ov.ID, again.ID, "overview replaced in place")
	assert.Equal(t, "second", doc.FileNotes("a.go")[0].Body)
	assert.Len(t, doc.FileNotes("a.go")[0].Thread, 1)

	_, err = doc.AddNote(" ", Note{Line: 1, Body: "x"})
	require.Error(t, err)
	_, err = doc.AddNote("a.go", Note{Line: 0, Body: "x"})
	require.Error(t, err)
}

func TestDocument_Answer(t *testing.T) {
	doc := &Document{Format: Format}
	n, err := doc.AddNote("a.go", Note{Line: 1, Body: "x"})
	require.NoError(t, err)
	note, _ := doc.Find(n.ID)
	note.addTurn(AuthorYou, "why?", time.Unix(1, 0))
	assert.Equal(t, 1, doc.PendingReplies())

	turn, err := doc.Answer(n.ID, " because ", time.Unix(2, 0))
	require.NoError(t, err)
	assert.Equal(t, AuthorClaude, turn.Author)
	assert.Equal(t, "because", turn.Body)
	assert.Equal(t, n.ID+".2", turn.ID)
	assert.Equal(t, TurnAnswered, note.Thread[0].State)
	assert.Equal(t, 0, doc.PendingReplies())

	_, err = doc.Answer("nope", "x", time.Unix(3, 0))
	require.Error(t, err)
	_, err = doc.Answer(n.ID, "  ", time.Unix(3, 0))
	require.Error(t, err)
}
