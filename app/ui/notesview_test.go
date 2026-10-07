package ui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/umputun/revdiff/app/keymap"
	"github.com/umputun/revdiff/app/notes"
	"github.com/umputun/revdiff/app/ui/style"
)

func notesColorResolver() style.Resolver {
	return style.NewResolver(style.Colors{
		Accent: "#5f87ff", Normal: "#d0d0d0", Muted: "#6c6c6c", Annotation: "#ffd700",
		AddBg: "#123800", AddFg: "#87d787", RemoveBg: "#4d1100", RemoveFg: "#ff8787", DiffBg: "#101010",
		CursorFg: "#bbbb44",
	})
}

func withColors(m Model) Model {
	res := notesColorResolver()
	m.resolver = res
	m.renderer = style.NewRenderer(res)
	m.invalidateRenderCaches()
	return m
}

func TestNotesView_CursorCell(t *testing.T) {
	m, _ := newNotesModel(t, notesTestDoc, 100, ModelConfig{})
	assert.Equal(t, noteGlyph, m.cursorCell(1, false), "plain glyph without colors")
	assert.Equal(t, " ", m.cursorCell(0, false))
	assert.Equal(t, m.renderer.DiffCursor(m.cfg.noColors), m.cursorCell(1, true), "the cursor wins over the marker")

	m = withColors(m)
	accent := string(m.resolver.Color(style.ColorKeyAccentFg))
	assert.Equal(t, accent+noteGlyph+string(style.ResetFg), m.cursorCell(1, false))
}

func TestNotesView_InlineRowsUseForegroundResetsOnly(t *testing.T) {
	m, _ := newNotesModel(t, notesTestDoc, 100, ModelConfig{})
	m = withColors(m)
	rows := m.inlineNoteRows(1)
	require.NotEmpty(t, rows)
	for _, r := range append(rows, m.overviewRows()...) {
		assert.NotContains(t, r, "\033[0m", "a full reset would break the pane background")
		assert.Equal(t, m.diffContentWidth()-m.gutterExtra(), lipgloss.Width(r), "padded to the content width")
	}
	assert.Contains(t, rows[0], string(m.resolver.Color(style.ColorKeyRemoveLineFg)), "a caution title uses the removed-line color")
	assert.Same(t, &rows[0], &m.inlineNoteRows(1)[0], "rows are memoized")
	assert.Nil(t, m.inlineNoteRows(0), "no notes, no rows")
}

func TestNotesView_NoteBlock(t *testing.T) {
	m, _ := newNotesModel(t, notesTestDoc, 100, ModelConfig{})
	n := notes.Note{ID: "n9", Kind: notes.KindExplain, Line: 3, Body: "first line\n\tsecond \x1b[31mred\x1b[0m line",
		Thread: []notes.Turn{
			{ID: "n9.1", Author: notes.AuthorYou, Body: "why?", State: notes.TurnPending},
			{ID: "n9.2", Author: notes.AuthorClaude, Body: "because"},
		}}
	rows := m.noteBlock(n, 40, false)
	plain := make([]string, len(rows))
	for i, r := range rows {
		plain[i] = r
		assert.LessOrEqual(t, lipgloss.Width(r), 40)
	}
	assert.Equal(t, "╭─ ◆ explain · n9 "+strings.Repeat("─", 40-lipgloss.Width("◆ explain · n9")-4), plain[0])
	assert.Equal(t, "│ first line", plain[1])
	assert.Equal(t, "│     second red line", plain[2], "tabs expand, escapes are stripped")
	assert.Equal(t, "│ you · pending:", plain[3], "no age inline: cached rows must not freeze one")
	assert.Equal(t, "│   why?", plain[4])
	assert.Equal(t, "│ claude:", plain[5])
	assert.Equal(t, "╰"+strings.Repeat("─", 39), plain[len(plain)-1])

	folded := m.noteBlock(n, 40, true)
	require.Len(t, folded, 1)
	assert.Contains(t, folded[0], "folded (- to unfold)")
}

func TestNotesView_WrapPlain(t *testing.T) {
	m, _ := newNotesModel(t, "", 100, ModelConfig{})
	assert.Equal(t, []string{""}, m.wrapPlain("", 10))
	assert.Equal(t, []string{"a", "b"}, m.wrapPlain("a\nb", 10))
	for _, l := range m.wrapPlain("one two three four five six seven", 10) {
		assert.LessOrEqual(t, lipgloss.Width(l), 10)
	}
}

func TestNotesView_TurnAge(t *testing.T) {
	m, _ := newNotesModel(t, "", 100, ModelConfig{})
	now := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	assert.Equal(t, "just now", m.turnAge(now.Add(-10*time.Second), now))
	assert.Equal(t, "5m ago", m.turnAge(now.Add(-5*time.Minute), now))
	assert.Equal(t, "3h ago", m.turnAge(now.Add(-3*time.Hour), now))

	lines := m.turnLines(notes.Turn{Author: notes.AuthorYou, Body: "q", At: now.Add(-5 * time.Minute), State: notes.TurnAnswered}, 40, now)
	assert.Equal(t, "you · 5m ago:", lines[0], "answered turns are not pending")
}

func TestNotesView_KeyHint(t *testing.T) {
	m, _ := newNotesModel(t, "", 100, ModelConfig{})
	assert.Equal(t, ")", m.keyHint(keymap.ActionNextNote))
	m.keymap.Unbind(")")
	assert.Equal(t, "next_note", m.keyHint(keymap.ActionNextNote), "unbound actions show their name")
}

func TestNotesView_PaneLines(t *testing.T) {
	m, _ := newNotesModel(t, notesTestDoc, 160, ModelConfig{})
	w := m.notesPaneWidth()
	check := func(lines []string) string {
		t.Helper()
		for _, l := range lines {
			assert.LessOrEqual(t, lipgloss.Width(l), w, "pane line %q", l)
		}
		return strings.Join(lines, "\n")
	}

	m.nav.diffCursor = 3
	text := check(m.notesPaneLines(w))
	assert.Contains(t, text, "▾ overview · n1")
	assert.Contains(t, text, "◆ explain · n3 · line 4 · 2/3")
	assert.Contains(t, text, "x moved")
	assert.Contains(t, text, "outdated (line not in the diff):")
	assert.Contains(t, text, "n4 explain · line 40")
	assert.Contains(t, text, "r reply · c annotate")

	m.notes.overviewCollapsed = true
	text = check(m.notesPaneLines(w))
	assert.Contains(t, text, "▸ overview · n1 (-)")
	assert.NotContains(t, text, "about a")

	m.modes.compact = true
	assert.Contains(t, check(m.notesPaneLines(w)), "not shown (compact view or outdated):")

	// a selected outdated note is shown faint
	m.notes.selected, m.notes.selectedAt = "n4", m.nav.diffCursor
	text = check(m.notesPaneLines(w))
	assert.Contains(t, text, "\033[2m")
	assert.Contains(t, text, "line is gone")

	// the overview's thread shows when the overview is the current note
	m.notes.overviewCollapsed = false
	_, err := m.notes.store.Reply("feature", "n1", "what about b?")
	require.NoError(t, err)
	m = pollNotesOnce(t, m)
	m.notes.selected, m.notes.selectedAt = "n1", m.nav.diffCursor
	text = check(m.notesPaneLines(w))
	assert.Contains(t, text, "what about b?")
	assert.Contains(t, text, "just now · pending")
}

func TestNotesView_PaneForFileWithoutNotes(t *testing.T) {
	doc := `{"format":"revdiff-notes/v1","files":[{"path":"b.go","overview":"only b"}]}`
	m, _ := newNotesModel(t, doc, 160, ModelConfig{})
	require.Equal(t, "a.go", m.file.name)
	assert.True(t, m.notesPaneVisible(), "the pane stays while the review has notes")
	text := strings.Join(m.notesPaneLines(m.notesPaneWidth()), "\n")
	assert.Contains(t, text, "No notes for this file.")
	assert.Equal(t, "Claude's notes", m.notesPaneTitle())
	assert.Equal(t, -1, m.currentNote())
	m, _ = pressRune(t, m, 'r')
	assert.False(t, m.notes.reply.active)
	assert.Contains(t, m.notes.hint, "No note in this file")
}

func TestNotesView_CurrentNoteNearest(t *testing.T) {
	m, _ := newNotesModel(t, notesTestDoc, 160, ModelConfig{})
	m.nav.diffCursor = 2 // between n2 (line idx 1) and n3 (line idx 3)
	assert.Equal(t, "n2", m.notes.file[m.currentNote()].ID, "the nearest note above wins")
	m.nav.diffCursor = 0
	assert.Equal(t, "n2", m.notes.file[m.currentNote()].ID, "else the nearest below")
	m.notes.at = nil
	assert.Equal(t, "n1", m.notes.file[m.currentNote()].ID, "else the overview")
}

func TestNotesView_CollapsedAndWrapModes(t *testing.T) {
	m, _ := newNotesModel(t, notesTestDoc, 100, ModelConfig{})
	m.modes.collapsed.enabled = true
	m.modes.collapsed.expandedHunks = map[int]bool{}
	m.modes.wrap = true
	out := m.renderDiff()
	assert.Contains(t, out, "╭─ ◆ overview · n1")
	assert.Contains(t, out, "careful with A")
	assert.Contains(t, out, noteGlyph+" + func A", "the marker leads a collapsed add line")

	rendered := strings.Split(strings.TrimSuffix(out, "\n"), "\n")
	hunks := m.findHunks()
	set := m.buildAnnotationSet()
	total := m.diffHeaderRows()
	for i := range m.file.lines {
		total += m.hunkLineHeight(i, hunks, set)
	}
	assert.Len(t, rendered, total, "collapsed heights include the inline notes")
}

func TestNotesView_ViewportWidthTracksNotes(t *testing.T) {
	m, h := newNotesModel(t, "", 160, ModelConfig{})
	full := m.layout.viewport.Width
	h.write(t, notesTestDoc)
	m = pollNotesOnce(t, m)
	assert.Less(t, m.layout.viewport.Width, full, "the pane takes room once notes arrive")
	assert.Equal(t, m.diffPaneWidth(), m.layout.viewport.Width)

	m, _ = feed(t, m, tea.WindowSizeMsg{Width: 120, Height: 30})
	assert.False(t, m.notesPaneVisible())
	assert.Equal(t, m.diffPaneWidth(), m.layout.viewport.Width)
	m, _ = pressRune(t, m, 't')
	assert.Equal(t, m.diffPaneWidth(), m.layout.viewport.Width, "tree toggle keeps the notes layout")
}
