package ui

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/umputun/revdiff/app/annotation"
	"github.com/umputun/revdiff/app/diff"
	"github.com/umputun/revdiff/app/keymap"
	"github.com/umputun/revdiff/app/notes"
	"github.com/umputun/revdiff/app/ui/mocks"
)

// notesTestDiffs: a.go has a context line, two added lines and a moved
// context line; b.go is one added line.
func notesTestDiffs() map[string][]diff.DiffLine {
	return map[string][]diff.DiffLine{
		"a.go": {
			{OldNum: 1, NewNum: 1, Content: "package a", ChangeType: diff.ChangeContext},
			{NewNum: 2, Content: "func A() {}", ChangeType: diff.ChangeAdd},
			{NewNum: 3, Content: "func B() {}", ChangeType: diff.ChangeAdd},
			{OldNum: 2, NewNum: 4, Content: "var x = 1", ChangeType: diff.ChangeContext},
		},
		"b.go": {{NewNum: 1, Content: "package b", ChangeType: diff.ChangeAdd}},
	}
}

const notesTestDoc = `{"format":"revdiff-notes/v1","tour":["b.go","a.go"],"files":[
 {"path":"a.go","overview":"about a","notes":[
   {"id":"n2","kind":"caution","line":2,"body":"careful with A"},
   {"id":"n3","line":4,"body":"x moved"},
   {"id":"n4","line":40,"body":"line is gone"}]},
 {"path":"b.go","notes":[{"id":"n5","line":1,"body":"b note"}]}]}`

// notesHarness is a model wired to real notes on disk (branch "feature" of
// the fake session store).
type notesHarness struct {
	repo *notes.Repo
	root string
}

func (h notesHarness) store() *notes.Store { return notes.New(filepath.Join(h.root, "feature")) }

func (h notesHarness) write(t *testing.T, doc string) {
	t.Helper()
	parsed, err := notes.Parse([]byte(doc))
	require.NoError(t, err)
	_, err = h.store().Update(func(d *notes.Document) error {
		d.Replace(parsed)
		return nil
	})
	require.NoError(t, err)
}

// newNotesModel builds a session-backed model over notesTestDiffs, writes doc
// (when not empty), loads the files, sizes the terminal and runs one notes poll.
func newNotesModel(t *testing.T, doc string, width int, cfg ModelConfig) (Model, notesHarness) {
	t.Helper()
	h := notesHarness{root: t.TempDir()}
	h.repo = notes.NewRepo(func(branch string) string { return filepath.Join(h.root, branch) })
	if doc != "" {
		h.write(t, doc)
	}
	cfg.Notes = h.repo
	m := sessionModelWith(t, newFakeSessionStore(), notesTestDiffs(), []string{"a.go", "b.go"}, cfg)
	m = loadAll(t, m)
	m, _ = feed(t, m, tea.WindowSizeMsg{Width: width, Height: 30})
	m = pollNotesOnce(t, m)
	return m, h
}

// pollNotesOnce runs one notes poll and applies it, dropping the next tick.
func pollNotesOnce(t *testing.T, m Model) Model {
	t.Helper()
	cmd := m.pollNotes(0)
	require.NotNil(t, cmd)
	m, _ = feed(t, m, cmd())
	return m
}

func pressKey(t *testing.T, m Model, msg tea.KeyMsg) Model {
	t.Helper()
	m, _ = feed(t, m, msg)
	return m
}

func typeText(t *testing.T, m Model, s string) Model {
	t.Helper()
	for _, r := range s {
		m, _ = pressRune(t, m, r)
	}
	return m
}

func TestNotes_StoreRequiresSessions(t *testing.T) {
	repo := notes.NewRepo(func(string) string { return t.TempDir() })
	m := testNewModel(t, plainRenderer(), annotation.NewStore(), noopHighlighter(), ModelConfig{Notes: repo})
	assert.Nil(t, m.notes.store, "notes live with sessions; no sessions, no notes")
	assert.Nil(t, m.pollNotes(0))
}

func TestNotes_AbsentLeavesViewUnchanged(t *testing.T) {
	for _, width := range []int{100, 160} {
		withNotes, _ := newNotesModel(t, "", width, ModelConfig{})
		require.True(t, withNotes.notes.loaded)
		plain := sessionModelWith(t, newFakeSessionStore(), notesTestDiffs(), []string{"a.go", "b.go"}, ModelConfig{})
		plain = loadAll(t, plain)
		plain, _ = feed(t, plain, tea.WindowSizeMsg{Width: width, Height: 30})
		assert.Equal(t, plain.View(), withNotes.View(), "no notes: byte-identical at width %d", width)
	}
}

func TestNotes_PollLocatesNotes(t *testing.T) {
	m, _ := newNotesModel(t, notesTestDoc, 160, ModelConfig{})
	require.NotNil(t, m.notes.doc)
	assert.Equal(t, "feature", m.notes.branch)
	assert.Equal(t, "a.go", m.file.name)
	require.Len(t, m.notes.file, 4)
	assert.Equal(t, []int{1}, m.notes.at[1], "caution on the added line 2")
	assert.Equal(t, []int{2}, m.notes.at[3], "explain on the context line with new number 4")
	assert.Equal(t, []int{3}, m.notes.lost, "line 40 is not in the diff")
	assert.Equal(t, map[string]int{"a.go": 4, "b.go": 1}, m.noteCounts())
	assert.Contains(t, m.View(), "◆4", "tree shows the note count")
}

func TestNotes_PollUnchangedStampSkipsLoad(t *testing.T) {
	m, h := newNotesModel(t, notesTestDoc, 160, ModelConfig{})
	msg := m.pollNotes(0)().(notesPolledMsg)
	assert.False(t, msg.changed)
	assert.Nil(t, msg.doc)
	assert.True(t, h.store().ViewerAlive(), "the poll marks the branch as viewed")

	// a poll result for a branch the review no longer shows is dropped
	m2, _ := feed(t, m, notesPolledMsg{branch: "other", changed: true})
	assert.Equal(t, "feature", m2.notes.branch)
	assert.NotNil(t, m2.notes.doc)
}

func TestNotes_PaneLayout(t *testing.T) {
	m, _ := newNotesModel(t, notesTestDoc, 160, ModelConfig{})
	require.True(t, m.notesPaneVisible())
	assert.False(t, m.notesInline())
	assert.Equal(t, m.diffPaneWidth(), m.layout.viewport.Width)
	assert.Equal(t, 160-m.layout.treeWidth-4-(m.notesPaneWidth()+2), m.diffPaneWidth())

	view := m.View()
	assert.Contains(t, view, "Claude's notes")
	assert.Contains(t, view, "about a", "overview pinned at the top of the pane")
	for i, line := range strings.Split(view, "\n") {
		assert.Equal(t, 160, lipgloss.Width(line), "row %d", i)
	}
	assert.NotContains(t, m.renderDiff(), "╭─", "no inline blocks while the pane shows the notes")
	assert.Contains(t, m.renderDiff(), noteGlyph, "the gutter marker stays")
}

func TestNotes_InlineWhenNarrow(t *testing.T) {
	m, _ := newNotesModel(t, notesTestDoc, 100, ModelConfig{})
	require.True(t, m.notesActive())
	assert.False(t, m.notesPaneVisible(), "100 columns cannot fit three panes")
	assert.True(t, m.notesInline())
	assert.Equal(t, 100-m.layout.treeWidth-4, m.diffPaneWidth())

	view := m.View()
	assert.NotContains(t, view, "Claude's notes")
	assert.Contains(t, view, "╭─ ◆ overview · n1")
	assert.Contains(t, view, "careful with A")
	for i, line := range strings.Split(view, "\n") {
		assert.Equal(t, 100, lipgloss.Width(line), "row %d", i)
	}
}

func TestNotes_TogglePane(t *testing.T) {
	m, _ := newNotesModel(t, notesTestDoc, 160, ModelConfig{})
	m, _ = pressRune(t, m, '>')
	assert.True(t, m.notes.paneHidden)
	assert.True(t, m.notesInline())
	assert.Equal(t, m.diffPaneWidth(), m.layout.viewport.Width)
	assert.Contains(t, m.renderDiff(), "careful with A")
	m, _ = pressRune(t, m, '>')
	assert.True(t, m.notesPaneVisible())

	narrow, _ := newNotesModel(t, notesTestDoc, 100, ModelConfig{})
	narrow, _ = pressRune(t, narrow, '>')
	narrow, _ = pressRune(t, narrow, '>')
	assert.Contains(t, narrow.notes.hint, "Too narrow")

	none, _ := newNotesModel(t, "", 160, ModelConfig{})
	none, _ = pressRune(t, none, '>')
	assert.Equal(t, "No notes from Claude for this review", none.notes.hint)
}

// TestNotes_InlineHeightsMatchRender checks the height bookkeeping that cursor
// and scroll math rely on against the rows actually painted.
func TestNotes_InlineHeightsMatchRender(t *testing.T) {
	m, _ := newNotesModel(t, notesTestDoc, 100, ModelConfig{})
	rendered := strings.Split(strings.TrimSuffix(m.renderDiff(), "\n"), "\n")
	hunks := m.findHunks()
	set := m.buildAnnotationSet()
	total := m.diffHeaderRows()
	for i := range m.file.lines {
		total += m.hunkLineHeight(i, hunks, set)
	}
	assert.Len(t, rendered, total)

	for i := range m.file.lines {
		m.nav.diffCursor = i
		y := m.cursorViewportY()
		require.Less(t, y, len(rendered))
		assert.Contains(t, rendered[y], m.file.lines[i].Content, "line %d starts at row %d", i, y)
		idx, onAnn := m.visualRowToDiffLine(y)
		assert.Equal(t, i, idx)
		assert.False(t, onAnn)
	}
	// rows of the inline block map back to their line, not to an annotation
	m.nav.diffCursor = 1
	y := m.cursorViewportY()
	idx, onAnn := m.visualRowToDiffLine(y + 1)
	assert.Equal(t, 1, idx)
	assert.False(t, onAnn)
	// rows of the overview map to the first line
	idx, _ = m.visualRowToDiffLine(0)
	assert.Equal(t, 0, idx)
}

func TestNotes_InlineWithAnnotationRows(t *testing.T) {
	m, _ := newNotesModel(t, notesTestDoc, 100, ModelConfig{})
	m.store.Add(annotation.Annotation{File: "a.go", Line: 2, Type: "+", Comment: "mine"})
	m.invalidateRenderCaches()
	rendered := strings.Split(strings.TrimSuffix(m.renderDiff(), "\n"), "\n")
	m.nav.diffCursor = 1
	y := m.cursorViewportY()
	assert.Contains(t, rendered[y], "func A() {}")
	assert.Contains(t, rendered[y+1], "mine", "the annotation sits right under its line")
	assert.Contains(t, rendered[y+2], "caution", "the note follows the annotation")
	idx, onAnn := m.visualRowToDiffLine(y + 1)
	assert.Equal(t, 1, idx)
	assert.True(t, onAnn)
	_, onAnn = m.visualRowToDiffLine(y + 2)
	assert.False(t, onAnn, "a note row is not the annotation sub-line")
}

func TestNotes_LiveReloadInvalidatesCache(t *testing.T) {
	for _, width := range []int{100, 160} {
		m, h := newNotesModel(t, notesTestDoc, width, ModelConfig{})
		m.nav.diffCursor = 3
		before := m.renderDiff() // warm the per-line cache
		assert.Contains(t, before, noteGlyph)

		h.write(t, strings.Replace(notesTestDoc, "careful with A", "careful with A, really", 1))
		h.write(t, strings.Replace(strings.Replace(notesTestDoc, "careful with A", "careful with A, really", 1),
			`{"id":"n3","line":4,"body":"x moved"},`, "", 1))
		m = pollNotesOnce(t, m)
		warm := m.renderDiff()
		m.invalidateRenderCaches()
		cold := m.renderDiff()
		assert.Equal(t, cold, warm, "a live reload must not paint cached rows (width %d)", width)
		assert.Empty(t, m.notes.at[3], "the removed note is gone")
		assert.Equal(t, 3, m.nav.diffCursor, "the cursor stays put")
		if width == 100 {
			assert.Contains(t, warm, "careful with A, really")
		}
	}
}

func TestNotes_NavigateTourOrder(t *testing.T) {
	m, _ := newNotesModel(t, notesTestDoc, 160, ModelConfig{})
	tour := m.noteTour()
	ids := make([]string, 0, len(tour))
	for _, tt := range tour {
		ids = append(ids, tt.file+":"+tt.id)
	}
	assert.Equal(t, []string{"b.go:n5", "a.go:n1", "a.go:n2", "a.go:n3", "a.go:n4"}, ids)

	// on a.go at its first line: ) goes to the first note after the cursor
	m.nav.diffCursor = 0
	m, _ = pressRune(t, m, ')')
	assert.Equal(t, 1, m.nav.diffCursor)
	assert.Equal(t, "n2", m.notes.selected)
	m, _ = pressRune(t, m, ')')
	assert.Equal(t, 3, m.nav.diffCursor)
	m, _ = pressRune(t, m, ')')
	assert.Equal(t, "n4", m.notes.selected, "the outdated note is selected without moving")
	assert.Equal(t, 3, m.nav.diffCursor)
	assert.Contains(t, m.notes.hint, "outdated")

	// ) wraps to the first tour file, loading it
	m, cmd := pressRune(t, m, ')')
	require.NotNil(t, cmd)
	assert.Equal(t, "n5", m.notes.pendingJump)
	m = settle(t, m, cmd)
	assert.Equal(t, "b.go", m.file.name)
	assert.Equal(t, "n5", m.notes.selected)
	assert.Equal(t, 0, m.nav.diffCursor)

	// ( goes back across files to the last note of a.go
	m, cmd = pressRune(t, m, '(')
	m = settle(t, m, cmd)
	assert.Equal(t, "a.go", m.file.name)
	assert.Equal(t, "n4", m.notes.selected)
	m, _ = pressRune(t, m, '(')
	assert.Equal(t, "n3", m.notes.selected)
	m, _ = pressRune(t, m, '(')
	m, _ = pressRune(t, m, '(')
	assert.Equal(t, "n1", m.notes.selected, "the overview is a stop")
	assert.False(t, m.notes.overviewCollapsed)
}

func TestNotes_OverviewFold(t *testing.T) {
	m, _ := newNotesModel(t, notesTestDoc, 100, ModelConfig{})
	open := len(m.overviewRows())
	assert.Greater(t, open, 1)
	m, _ = pressRune(t, m, '-')
	assert.True(t, m.notes.overviewCollapsed)
	assert.Len(t, m.overviewRows(), 1)
	assert.Contains(t, m.renderDiff(), "folded (- to unfold)")
	m, _ = pressRune(t, m, '-')
	assert.Len(t, m.overviewRows(), open)
}

func TestNotes_Reply(t *testing.T) {
	m, h := newNotesModel(t, notesTestDoc, 160, ModelConfig{})
	m.nav.diffCursor = 1
	m, _ = pressRune(t, m, 'r')
	require.True(t, m.notes.reply.active)
	assert.Equal(t, "n2", m.notes.reply.noteID)
	assert.Contains(t, m.statusBarText(), "◆ reply to n2:")

	m = typeText(t, m, "why careful?")
	m = pressKey(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	assert.False(t, m.notes.reply.active)
	assert.Equal(t, "Reply saved — no agent is listening (Claude reads it with revdiff inbox)", m.notes.hint)
	n, _ := m.notes.doc.Find("n2")
	require.Len(t, n.Thread, 1)
	assert.Equal(t, "why careful?", n.Thread[0].Body)
	assert.Contains(t, m.View(), "why careful?", "the reply shows in the pane at once")

	m = pressKey(t, m, tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}}) // clears the hint
	status := m.statusBarText()
	assert.Contains(t, status, "1 reply pending")
	assert.Contains(t, status, "no agent listening")

	items, err := h.store().Inbox()
	require.NoError(t, err)
	require.Len(t, items, 1)
	assert.Equal(t, "careful with A", items[0].Note.Body)

	// with a listener the hint and the status change
	require.NoError(t, h.store().MarkListening())
	m = pollNotesOnce(t, m)
	assert.True(t, m.notes.listener)
	assert.NotContains(t, m.statusBarText(), "no agent listening")
	m.nav.diffCursor = 1
	m, _ = pressRune(t, m, 'r')
	m = typeText(t, m, "more")
	m = pressKey(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	assert.Equal(t, "Reply sent to Claude", m.notes.hint)
	h.store().UnmarkListening()
}

func TestNotes_ReplyCancel(t *testing.T) {
	m, _ := newNotesModel(t, notesTestDoc, 160, ModelConfig{})
	m, _ = pressRune(t, m, 'r')
	m = typeText(t, m, "abc")
	m = pressKey(t, m, tea.KeyMsg{Type: tea.KeyEsc})
	assert.False(t, m.notes.reply.active)
	assert.Equal(t, "Reply canceled", m.notes.hint)

	m, _ = pressRune(t, m, 'r')
	m = pressKey(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	assert.Equal(t, "Reply canceled", m.notes.hint, "an empty reply is not sent")
	assert.Zero(t, m.notes.doc.PendingReplies())

	// mouse events are swallowed while replying
	m, _ = pressRune(t, m, 'r')
	cursor := m.nav.diffCursor
	m, _ = feed(t, m, tea.MouseMsg{X: 50, Y: 10, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress})
	assert.Equal(t, cursor, m.nav.diffCursor)
	assert.True(t, m.notes.reply.active)
}

func TestNotes_ReplyEditor(t *testing.T) {
	ed := &mocks.ExternalEditorMock{CommandFunc: func(content string) (*exec.Cmd, func(error) (string, error), error) {
		return exec.Command("true"), func(error) (string, error) { return "from editor", nil }, nil
	}}
	m, _ := newNotesModel(t, notesTestDoc, 160, ModelConfig{Editor: ed})
	m.nav.diffCursor = 1
	m, _ = pressRune(t, m, 'r')
	m = typeText(t, m, "draft")
	m, cmd := feed(t, m, tea.KeyMsg{Type: tea.KeyCtrlE})
	require.NotNil(t, cmd)
	assert.False(t, m.notes.reply.active, "the draft moved to the editor")
	require.Len(t, ed.CommandCalls(), 1)
	assert.Equal(t, "draft", ed.CommandCalls()[0].Content)

	m, _ = feed(t, m, replyEditorFinishedMsg{noteID: "n2", content: "from editor\n"})
	n, _ := m.notes.doc.Find("n2")
	require.Len(t, n.Thread, 1)
	assert.Equal(t, "from editor", n.Thread[0].Body)

	m, _ = feed(t, m, replyEditorFinishedMsg{noteID: "n2", err: assert.AnError})
	assert.Contains(t, m.notes.hint, "Reply editor failed")
}

func TestNotes_ReplyWithoutStatusBarUsesEditor(t *testing.T) {
	ed := &mocks.ExternalEditorMock{CommandFunc: func(string) (*exec.Cmd, func(error) (string, error), error) {
		return exec.Command("true"), func(error) (string, error) { return "", nil }, nil
	}}
	m, _ := newNotesModel(t, notesTestDoc, 160, ModelConfig{Editor: ed, NoStatusBar: true})
	m.nav.diffCursor = 1
	m, cmd := pressRune(t, m, 'r')
	require.NotNil(t, cmd)
	assert.False(t, m.notes.reply.active)
	assert.Len(t, ed.CommandCalls(), 1)
}

func TestNotes_BranchSwitchDropsNotes(t *testing.T) {
	m, _ := newNotesModel(t, notesTestDoc, 160, ModelConfig{})
	require.NotNil(t, m.notes.doc)
	m.session.cur.Branch = "other"
	m, _ = feed(t, m, notesPolledMsg{branch: "feature", changed: true})
	assert.Nil(t, m.notes.doc, "the previous branch's notes are not shown on another branch")
	assert.False(t, m.notesPaneVisible())
	m = pollNotesOnce(t, m)
	assert.Equal(t, "other", m.notes.branch)
	assert.Nil(t, m.notes.doc, "the other branch has no notes")
}

func TestNotes_NoteToAnnotation(t *testing.T) {
	m, _ := newNotesModel(t, notesTestDoc, 160, ModelConfig{})
	m.nav.diffCursor = 1
	m, _ = pressRune(t, m, 'c')
	require.True(t, m.annot.annotating)
	assert.Equal(t, noteSuggestionKind, m.annot.kind)
	assert.Equal(t, "re Claude's note “careful with A”: ", m.annot.input.Value())
	m = typeText(t, m, "rename it")
	m = pressKey(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	anns := m.store.Get("a.go")
	require.Len(t, anns, 1)
	assert.Equal(t, 2, anns[0].Line)
	assert.Equal(t, "+", anns[0].Type)
	assert.Equal(t, noteSuggestionKind, anns[0].Kind)
	assert.Equal(t, "re Claude's note “careful with A”: rename it", anns[0].Comment)
	assert.Contains(t, m.store.FormatOutput(), "suggestion: re Claude's note")

	// an existing annotation is opened unchanged
	m.nav.diffCursor = 1
	m.annot.cursorOnAnnotation = false
	m, _ = pressRune(t, m, 'c')
	assert.Equal(t, "re Claude's note “careful with A”: rename it", m.annot.input.Value())
	m = pressKey(t, m, tea.KeyMsg{Type: tea.KeyEsc})

	// the overview becomes a file-level annotation
	m.nav.diffCursor = 0
	m.notes.selected = ""
	m.notes.at = map[int][]int{} // nothing on a line: the overview is current
	m, _ = pressRune(t, m, 'c')
	require.True(t, m.annot.annotating)
	assert.True(t, m.annot.fileAnnotating)
	assert.Equal(t, "re Claude's note “about a”: ", m.annot.input.Value())
}

func TestNotes_NoteToAnnotationOutdated(t *testing.T) {
	m, _ := newNotesModel(t, notesTestDoc, 160, ModelConfig{})
	m.notes.selected, m.notes.selectedAt = "n4", m.nav.diffCursor
	m, _ = pressRune(t, m, 'c')
	assert.False(t, m.annot.annotating)
	assert.Contains(t, m.notes.hint, "outdated")
}

func TestNotes_KeysWithoutNotes(t *testing.T) {
	m, _ := newNotesModel(t, "", 160, ModelConfig{})
	for _, r := range []rune{')', '(', 'r', 'c', '-'} {
		m, _ = pressRune(t, m, r)
		assert.Equal(t, "No notes from Claude for this review", m.notes.hint, "key %q", r)
		assert.False(t, m.annot.annotating)
		assert.False(t, m.notes.reply.active)
	}
}

func TestNotes_NeverInAnnotationOutput(t *testing.T) {
	m, h := newNotesModel(t, notesTestDoc, 160, ModelConfig{})
	_, err := h.store().Reply("n2", "question")
	require.NoError(t, err)
	m = pollNotesOnce(t, m)
	assert.Empty(t, m.store.FormatOutput())
	assert.Zero(t, m.store.Count())
}

func TestNotes_MouseInPane(t *testing.T) {
	long := strings.Replace(notesTestDoc, `"overview":"about a"`, `"overview":"`+strings.Repeat("long overview text ", 120)+`"`, 1)
	m, _ := newNotesModel(t, long, 160, ModelConfig{})
	x := 160 - 3
	assert.Equal(t, hitNotes, m.hitTest(x, 5))
	assert.Equal(t, hitDiff, m.hitTest(160-m.notesPaneCols()-3, 5))

	m, _ = feed(t, m, tea.MouseMsg{X: x, Y: 5, Button: tea.MouseButtonWheelDown, Action: tea.MouseActionPress})
	assert.Equal(t, wheelStep, m.notes.scroll)
	for range 200 {
		m, _ = feed(t, m, tea.MouseMsg{X: x, Y: 5, Button: tea.MouseButtonWheelDown, Action: tea.MouseActionPress})
	}
	assert.Equal(t, len(m.notesPaneLines(m.notesPaneWidth()))-(m.paneHeight()-1), m.notes.scroll, "clamped at the end")
	m, _ = feed(t, m, tea.MouseMsg{X: x, Y: 5, Button: tea.MouseButtonWheelUp, Action: tea.MouseActionPress})
	assert.Less(t, m.notes.scroll, len(m.notesPaneLines(m.notesPaneWidth())))

	cursor := m.nav.diffCursor
	m, _ = feed(t, m, tea.MouseMsg{X: x, Y: 5, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress})
	assert.Equal(t, cursor, m.nav.diffCursor, "a click in the notes pane does not reach the diff")
	for i, line := range strings.Split(m.View(), "\n") {
		assert.Equal(t, 160, lipgloss.Width(line), "row %d with a scrolled pane", i)
	}
}

func TestNotes_LoadErrorHint(t *testing.T) {
	m, h := newNotesModel(t, "", 160, ModelConfig{})
	m.notes.loaded = false
	require.NoError(t, os.MkdirAll(filepath.Join(h.root, "feature", "notes"), 0o700))
	require.NoError(t, os.WriteFile(filepath.Join(h.root, "feature", "notes", "notes.json"), []byte("{"), 0o600))
	m = pollNotesOnce(t, m)
	assert.Contains(t, m.notes.hint, "could not be read")
	assert.Nil(t, m.notes.doc)
}

func TestNotes_HelpListsNotesKeys(t *testing.T) {
	m, _ := newNotesModel(t, notesTestDoc, 160, ModelConfig{})
	spec := m.buildHelpSpec()
	var keys []string
	for _, sec := range spec.Sections {
		if sec.Title != "Notes" {
			continue
		}
		for _, e := range sec.Entries {
			keys = append(keys, e.Keys)
		}
	}
	assert.Len(t, keys, 6)
	for _, a := range []keymap.Action{keymap.ActionToggleNotes, keymap.ActionNextNote, keymap.ActionReplyNote} {
		assert.NotEmpty(t, m.keymap.KeysFor(a))
	}
}
