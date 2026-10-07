package ui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"

	"github.com/umputun/revdiff/app/diff"
	"github.com/umputun/revdiff/app/keymap"
	"github.com/umputun/revdiff/app/notes"
	"github.com/umputun/revdiff/app/ui/style"
)

// Claude's notes reuse existing theme colors instead of adding keys: the frame,
// the ◆ marker and Claude's name use the accent color (pane borders), note
// text the normal color, a caution label the removed-line color, and the
// reviewer's own turns the annotation color, so they read as "you" the same
// way the reviewer's annotations do. Outdated notes are drawn faint.

// noteGlyph marks a line with notes in the cursor column and labels notes.
const noteGlyph = "◆"

// noteSuggestionKind is the annotation kind a note turned into a change
// request gets (the `c` key).
const noteSuggestionKind = "suggestion"

// noteRowsKey memoizes one inline note block: the render key of the notes it
// paints (or of the overview), the width and the overview fold state.
type noteRowsKey struct {
	key       string
	width     int
	collapsed bool
}

// cursorCell returns the one-column cell left of a diff line: the cursor bar
// on the cursor line, the ◆ marker on a line with notes, else a space.
func (m Model) cursorCell(idx int, isCursor bool) string {
	if isCursor {
		return m.renderer.DiffCursor(m.cfg.noColors)
	}
	if len(m.notes.at[idx]) > 0 {
		return m.paint(style.ColorKeyAccentFg, noteGlyph)
	}
	return " "
}

// paint wraps text in a theme foreground color with a foreground-only reset,
// so surrounding backgrounds survive. No color (--no-colors) returns text.
func (m Model) paint(k style.ColorKey, text string) string {
	c := m.resolver.Color(k)
	if c == "" || text == "" {
		return text
	}
	return string(c) + text + string(style.ResetFg)
}

// noteContentWidth is the width an inline note block occupies right of the
// cursor column, matching the annotation rows above it.
func (m Model) noteContentWidth() int {
	return max(m.diffContentWidth()-1, wrapMinContent+4)
}

// inlineNoteRows returns the rendered rows of the notes drawn under diff line
// idx (cursor column and background fill included), or nil when notes are in
// the pane or the line has none. It is the single source for both the height
// (hunkLineHeight) and the painter (renderInlineNotes).
func (m Model) inlineNoteRows(idx int) []string {
	list := m.notes.at[idx]
	if len(list) == 0 || !m.notesInline() {
		return nil
	}
	key := noteRowsKey{key: m.notes.keys[idx], width: m.diffContentWidth()}
	if rows, ok := m.notes.rowCache[key]; ok {
		return rows
	}
	w := m.noteContentWidth()
	var rows []string
	for _, i := range list {
		rows = append(rows, m.noteBlock(m.notes.file[i], w, false)...)
	}
	rows = m.finishInlineRows(rows)
	if m.notes.rowCache != nil {
		m.notes.rowCache[key] = rows
	}
	return rows
}

// overviewRows returns the rendered rows of the current file's overview at
// the top of the diff (inline mode only), folded to one row by `-`.
func (m Model) overviewRows() []string {
	if !m.notesInline() {
		return nil
	}
	ov := m.fileOverview()
	if ov == nil {
		return nil
	}
	key := noteRowsKey{key: "overview\x00" + m.noteRenderKey(*ov), width: m.diffContentWidth(), collapsed: m.notes.overviewCollapsed}
	if rows, ok := m.notes.rowCache[key]; ok {
		return rows
	}
	rows := m.finishInlineRows(m.noteBlock(*ov, m.noteContentWidth(), m.notes.overviewCollapsed))
	if m.notes.rowCache != nil {
		m.notes.rowCache[key] = rows
	}
	return rows
}

// finishInlineRows prepends the cursor column and pads every row with the
// diff pane background, like annotation rows.
func (m Model) finishInlineRows(rows []string) []string {
	paneBg := m.resolver.Color(style.ColorKeyDiffPaneBg)
	out := make([]string, len(rows))
	for i, r := range rows {
		out[i] = m.extendLineBg(" "+r, paneBg)
	}
	return out
}

// renderInlineNotes writes the inline notes under diff line idx.
func (m Model) renderInlineNotes(b *strings.Builder, idx int) {
	for _, r := range m.inlineNoteRows(idx) {
		b.WriteString(r + "\n")
	}
}

// renderNotesHeader writes the inline overview below the file annotation.
func (m Model) renderNotesHeader(b *strings.Builder) {
	for _, r := range m.overviewRows() {
		b.WriteString(r + "\n")
	}
}

// diffHeaderRows is the number of rows above the first diff line: the
// file-level annotation (when present) and the inline overview.
func (m Model) diffHeaderRows() int {
	n := 0
	if m.hasFileAnnotation() {
		n = m.wrappedAnnotationLineCount(annotKeyFile)
	}
	return n + len(m.overviewRows())
}

// noteBlock frames one note for inline display at width w:
//
//	╭─ ◆ caution · n3 ────────
//	│ body
//	│ you · pending: question
//	│ claude: answer
//	╰─────────────────────────
//
// folded shows only the title row (the overview under `-`).
func (m Model) noteBlock(n notes.Note, w int, folded bool) []string {
	title := m.noteTitle(n)
	if folded {
		title += " · folded (" + m.keyHint(keymap.ActionToggleOverview) + " to unfold)"
	}
	titleW := lipgloss.Width(title)
	top := m.paint(style.ColorKeyAccentFg, "╭─ ") + m.styledNoteTitle(n, title) + " " +
		m.paint(style.ColorKeyAccentFg, strings.Repeat("─", max(0, w-titleW-4)))
	if folded {
		return []string{top}
	}
	rows := []string{top}
	bar := m.paint(style.ColorKeyAccentFg, "│") + " "
	for _, line := range m.wrapPlain(m.noteText(n.Body), w-2) {
		rows = append(rows, bar+m.paint(style.ColorKeyNormalFg, line))
	}
	for _, t := range n.Thread {
		// no age inline: the rows are cached, and a relative age would freeze
		for _, line := range m.turnLines(t, w-2, time.Time{}) {
			rows = append(rows, bar+line)
		}
	}
	rows = append(rows, m.paint(style.ColorKeyAccentFg, "╰"+strings.Repeat("─", max(0, w-1))))
	return rows
}

// noteTitle is the plain title of a note: glyph, kind and id.
func (m Model) noteTitle(n notes.Note) string {
	return noteGlyph + " " + string(n.Kind) + " · " + n.ID
}

// styledNoteTitle colors a note title: a caution in the removed-line color,
// other kinds in the accent color.
func (m Model) styledNoteTitle(n notes.Note, title string) string {
	if n.Kind == notes.KindCaution {
		return m.paint(style.ColorKeyRemoveLineFg, title)
	}
	return m.paint(style.ColorKeyAccentFg, title)
}

// turnLines renders one thread turn as wrapped lines prefixed by its author
// and, unless now is zero, its age.
func (m Model) turnLines(t notes.Turn, w int, now time.Time) []string {
	label := "claude"
	key := style.ColorKeyAccentFg
	if t.Author == notes.AuthorYou {
		label, key = "you", style.ColorKeyAnnotationFg
	}
	if !t.At.IsZero() && !now.IsZero() {
		label += " · " + m.turnAge(t.At, now)
	}
	if t.Author == notes.AuthorYou && t.State != notes.TurnAnswered {
		label += " · pending"
	}
	label += ":"
	indent := "  "
	lines := m.wrapPlain(m.noteText(t.Body), max(wrapMinContent, w-lipgloss.Width(indent)))
	out := make([]string, 0, len(lines)+1)
	out = append(out, m.paint(key, label))
	for _, l := range lines {
		out = append(out, indent+m.paint(style.ColorKeyNormalFg, l))
	}
	return out
}

// turnAge describes when a thread turn was written: "just now" under a
// minute, else the blame gutter's compact age ("5m ago", "3h ago").
func (m Model) turnAge(at, now time.Time) string {
	if now.Sub(at) < time.Minute {
		return "just now"
	}
	return strings.TrimSpace(diff.RelativeAge(at, now)) + " ago"
}

// noteText makes agent-written text safe to paint: control and escape
// sequences are stripped (newlines kept) and tabs become spaces.
func (m Model) noteText(s string) string {
	s = diff.SanitizeCommitText(s)
	return strings.TrimRight(strings.ReplaceAll(s, "\t", m.cfg.tabSpaces), "\n ")
}

// wrapPlain wraps plain text at w, keeping its own line breaks. An empty
// text yields one empty line.
func (m Model) wrapPlain(s string, w int) []string {
	var out []string
	for para := range strings.SplitSeq(s, "\n") {
		if w <= 0 || lipgloss.Width(para) <= w {
			out = append(out, para)
			continue
		}
		out = append(out, m.wrapContent(para, w)...)
	}
	return out
}

// keyHint returns a key bound to action for inline hints, or the action
// name when it is unbound.
func (m Model) keyHint(action keymap.Action) string {
	if keys := m.keymap.KeysFor(action); len(keys) > 0 {
		return m.displayKeyName(keys[0])
	}
	return string(action)
}

// renderNotesPane renders the notes pane (borders included) at the pane height.
func (m Model) renderNotesPane(ph int) string {
	w := m.notesPaneWidth()
	lines := m.notesPaneLines(w)
	body := ph - 1 // the header row
	scroll := min(m.notes.scroll, max(0, len(lines)-body))
	end := min(len(lines), scroll+body)
	header := m.resolver.Style(style.StyleKeyDirEntry).Render(m.truncateHeaderTitle(m.notesPaneTitle(), w))
	content := strings.Join(append([]string{header}, lines[scroll:end]...), "\n")
	content = m.padContentBg(content, w, m.resolver.Color(style.ColorKeyTreePaneBg))
	return m.resolver.Style(style.StyleKeyTreePane).Width(w).Height(ph).Render(content)
}

// notesPaneTitle is the single-row header of the notes pane.
func (m Model) notesPaneTitle() string {
	title := "Claude's notes"
	if n := len(m.notes.file); n > 0 {
		title += fmt.Sprintf(" · %d here", n)
	}
	return title
}

// notesPaneLines lays out the pane body for the current file: the overview
// (folded by `-`), the current note with its thread, and the notes that could
// not be placed. Every line fits in width w.
func (m Model) notesPaneLines(w int) []string {
	tw := max(1, w-1)
	var out []string
	add := func(s string) { out = append(out, " "+s) }
	if len(m.notes.file) == 0 {
		add(m.paint(style.ColorKeyMutedFg, "No notes for this file."))
		add("")
		add(m.paint(style.ColorKeyMutedFg, m.keyHint(keymap.ActionNextNote)+" next note · "+m.keyHint(keymap.ActionToggleNotes)+" hide pane"))
		return out
	}
	cur := m.currentNote()
	if ov := m.fileOverview(); ov != nil {
		head := "▾ overview · " + ov.ID
		if m.notes.overviewCollapsed {
			head = "▸ overview · " + ov.ID + " (" + m.keyHint(keymap.ActionToggleOverview) + ")"
		}
		add(m.paint(style.ColorKeyAccentFg, m.truncateRight(head, tw)))
		if !m.notes.overviewCollapsed {
			for _, l := range m.wrapPlain(m.noteText(ov.Body), tw) {
				add(m.paint(style.ColorKeyNormalFg, l))
			}
			if cur >= 0 && m.notes.file[cur].ID == ov.ID {
				for _, t := range ov.Thread {
					for _, l := range m.turnLines(t, tw, time.Now()) {
						add(l)
					}
				}
			}
		}
		add("")
	}
	if cur >= 0 && m.notes.file[cur].Kind != notes.KindOverview {
		out = append(out, m.paneNoteLines(cur, tw)...)
		add("")
	}
	if len(m.notes.lost) > 0 {
		label := "outdated (line not in the diff):"
		if m.modes.compact {
			label = "not shown (compact view or outdated):"
		}
		add(m.paint(style.ColorKeyMutedFg, m.truncateRight(label, tw)))
		for _, i := range m.notes.lost {
			n := m.notes.file[i]
			add(m.paint(style.ColorKeyMutedFg, m.truncateRight(fmt.Sprintf("  %s %s · line %d", n.ID, n.Kind, n.Line), tw)))
		}
		add("")
	}
	add(m.paint(style.ColorKeyMutedFg, m.truncateRight(m.keyHint(keymap.ActionReplyNote)+" reply · "+m.keyHint(keymap.ActionNoteToAnnotation)+
		" annotate · "+m.keyHint(keymap.ActionNextNote)+"/"+m.keyHint(keymap.ActionPrevNote)+" next/prev", tw)))
	return out
}

// paneNoteLines renders note i of the current file for the pane: its title
// with line and position, body and thread.
func (m Model) paneNoteLines(i, tw int) []string {
	n := m.notes.file[i]
	var out []string
	add := func(s string) { out = append(out, " "+s) }
	pos, total := 0, 0
	for j, other := range m.notes.file {
		if other.Kind == notes.KindOverview {
			continue
		}
		total++
		if j == i {
			pos = total
		}
	}
	title := fmt.Sprintf("%s · line %d", m.noteTitle(n), n.Line)
	if n.Side == notes.SideOld {
		title += " (removed)"
	}
	title += fmt.Sprintf(" · %d/%d", pos, total)
	add(m.styledNoteTitle(n, m.truncateRight(title, tw)))
	faint := m.noteLine(i) < 0
	for _, l := range m.wrapPlain(m.noteText(n.Body), tw) {
		line := m.paint(style.ColorKeyNormalFg, l)
		if faint {
			line = "\033[2m" + line + "\033[22m"
		}
		add(line)
	}
	for _, t := range n.Thread {
		for _, l := range m.turnLines(t, tw, time.Now()) {
			add(l)
		}
	}
	return out
}

// truncateRight cuts s (which may carry ANSI) to w display columns, ending in
// an ellipsis when it was longer.
func (m Model) truncateRight(s string, w int) string {
	return ansi.Truncate(s, w, "…")
}

// scrollNotesPane moves the notes pane by delta rows, clamped to its content.
func (m *Model) scrollNotesPane(delta int) {
	body := max(1, m.paneHeight()-1)
	maxScroll := max(0, len(m.notesPaneLines(m.notesPaneWidth()))-body)
	m.notes.scroll = min(max(0, m.notes.scroll+delta), maxScroll)
}
