package ui

import (
	"fmt"
	"regexp"
	"slices"
	"strings"

	"github.com/charmbracelet/bubbles/cursor"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/umputun/revdiff/app/annotation"
	"github.com/umputun/revdiff/app/diff"
	"github.com/umputun/revdiff/app/keymap"
	"github.com/umputun/revdiff/app/ui/style"
)

// annotKeyFile is the lookup key for file-level annotations in wrappedAnnotationLineCount.
const annotKeyFile = "file"

// annotPrefix returns the cached annotation line prefix (marker + space).
func (m Model) annotPrefix() string {
	return m.cfg.annotPrefix
}

// annotFilePrefix returns the cached file-level annotation prefix (marker + " file: ").
func (m Model) annotFilePrefix() string {
	return m.cfg.annotFilePrefix
}

// annotCharLimit caps annotation text length. sized for multi-item lists and
// small pasted data slices, not for full-document content.
const annotCharLimit = 8000

// hunkKeywordRe matches whole-word "hunk" (case-insensitive).
// "block" was removed as it triggers false positives in casual usage (e.g., "this code block is fine").
var hunkKeywordRe = regexp.MustCompile(`(?i)\bhunk\b`)

// newAnnotationInput creates and focuses a text input for annotation editing.
// prefixWidth accounts for the visible prefix characters (cursor col + emoji + label + margin).
func (m *Model) newAnnotationInput(placeholder string, prefixWidth int) (textinput.Model, tea.Cmd) {
	ti := textinput.New()
	ti.Placeholder = placeholder
	// static cursor, set before Focus so no blink command is scheduled. the input is painted
	// inside renderDiff, so a blink could only become visible by re-rendering every diff line
	// (~7us per line) twice a second for a session that is otherwise idle. a static cursor is
	// what the user already sees between blinks on a large diff.
	ti.Cursor.SetMode(cursor.CursorStatic)
	cmd := ti.Focus()
	ti.CharLimit = annotCharLimit
	ti.Width = max(10, m.diffContentWidth()-prefixWidth)

	// set DiffBg on all textinput sub-styles so View() output inherits the pane background.
	// wrapping View() externally doesn't work because lipgloss Render emits \033[0m resets.
	inputStyle := m.resolver.Style(style.StyleKeyAnnotInputText)
	ti.PromptStyle = inputStyle
	ti.TextStyle = inputStyle
	cursorStyle := m.resolver.Style(style.StyleKeyAnnotInputCursor)
	ti.Cursor.TextStyle = cursorStyle
	ti.Cursor.Style = cursorStyle
	ti.PlaceholderStyle = m.resolver.Style(style.StyleKeyAnnotInputPlaceholder)

	return ti, cmd
}

// startAnnotation enters annotation input mode for the current cursor line.
func (m *Model) startAnnotation() tea.Cmd {
	m.clearPendingInputState()
	dl, ok := m.cursorDiffLine()
	if !ok || dl.ChangeType == diff.ChangeDivider {
		return nil
	}
	// prevent annotating hidden or placeholder removed lines in collapsed mode
	hunks := m.findHunks()
	if m.isCollapsedHidden(m.nav.diffCursor, hunks) {
		return nil
	}
	if m.isDeleteOnlyPlaceholder(m.nav.diffCursor, hunks) {
		return nil
	}

	editorKey := m.editorKeyDisplay()
	placeholder := "annotation..."
	if editorKey != "" {
		placeholder = fmt.Sprintf("annotation... (%s for editor)", editorKey)
	}

	// pre-fill with existing annotation if one exists. multi-line comments are
	// NOT set via ti.SetValue because textinput's sanitizer collapses \n to
	// space; instead, stash the original in existingMultiline and hint at it
	// via the placeholder so the editor key can seed the editor from it and
	// Enter with empty input preserves it unchanged.
	lineNum := m.diffLineNum(dl)
	var preFill, existingMultiline, kind string
	for _, a := range m.store.Get(m.file.name) {
		if a.Line != lineNum || a.Type != string(dl.ChangeType) {
			continue
		}
		kind = a.Kind
		if strings.Contains(a.Comment, "\n") {
			existingMultiline = a.Comment
			placeholder = m.multiLinePlaceholder()
		} else {
			preFill = a.Comment
		}
		break
	}

	m.annot.fileAnnotating = false
	m.annot.kind = kind
	ti, cmd := m.newAnnotationInput(placeholder, m.annotInputPrefixWidth())
	if preFill != "" {
		ti.SetValue(preFill)
	}

	m.annot.input = ti
	m.annot.annotating = true
	m.annot.existingMultiline = existingMultiline
	m.ensureLineAnnotationInputVisible()
	return cmd
}

// ensureLineAnnotationInputVisible scrolls the viewport so the line-annotation
// input row is visible. the input is rendered below the diff line, so keeping
// the cursor line visible is not always sufficient when cursor is on the last
// visible row.
func (m *Model) ensureLineAnnotationInputVisible() {
	if !m.annot.annotating || m.annot.fileAnnotating || m.layout.viewport.Height <= 0 {
		return
	}
	if m.nav.diffCursor < 0 || m.nav.diffCursor >= len(m.file.lines) {
		return
	}

	inputY := m.cursorViewportY() + m.wrappedLineCount(m.nav.diffCursor)
	switch {
	case inputY < m.layout.viewport.YOffset:
		m.layout.viewport.SetYOffset(inputY)
	case inputY >= m.layout.viewport.YOffset+m.layout.viewport.Height:
		m.layout.viewport.SetYOffset(inputY - m.layout.viewport.Height + 1)
	}
}

// startFileAnnotation enters annotation input mode for a file-level annotation (Line=0).
func (m *Model) startFileAnnotation() tea.Cmd {
	m.clearPendingInputState()
	if m.file.name == "" {
		return nil
	}

	editorKey := m.editorKeyDisplay()
	placeholder := "file-level annotation..."
	if editorKey != "" {
		placeholder = fmt.Sprintf("file-level annotation... (%s for editor)", editorKey)
	}

	// pre-fill with existing file-level annotation if one exists. multi-line
	// comments bypass ti.SetValue (textinput sanitizer flattens \n to space);
	// instead stash in existingMultiline so the editor key can seed and Enter
	// with empty input preserves it unchanged.
	var preFill, existingMultiline, kind string
	for _, a := range m.store.Get(m.file.name) {
		if a.Line != 0 {
			continue
		}
		kind = a.Kind
		if strings.Contains(a.Comment, "\n") {
			existingMultiline = a.Comment
			placeholder = m.multiLinePlaceholder()
		} else {
			preFill = a.Comment
		}
		break
	}

	m.annot.fileAnnotating = true
	m.annot.kind = kind
	ti, cmd := m.newAnnotationInput(placeholder, m.annotInputPrefixWidth())
	if preFill != "" {
		ti.SetValue(preFill)
	}

	m.annot.input = ti
	m.annot.annotating = true
	m.annot.existingMultiline = existingMultiline
	m.nav.diffCursor = -1 // position cursor on the file annotation line
	m.layout.viewport.GotoTop()
	return cmd
}

// saveAnnotation saves the current text input as an annotation on the cursor line.
// Thin wrapper around saveComment that reads model state for the current target.
//
// Empty input: an existing multi-line comment is preserved (only its kind is
// updated), a selected kind saves a kind-only annotation, and with neither the
// input is canceled.
func (m *Model) saveAnnotation() {
	text := m.annot.input.Value()
	fileLevel := m.annot.fileAnnotating
	var line int
	var changeType string
	if !fileLevel {
		dl, ok := m.cursorDiffLine()
		if !ok {
			m.cancelAnnotation()
			return
		}
		line, changeType = m.diffLineNum(dl), string(dl.ChangeType)
	}

	if text == "" && m.annot.existingMultiline != "" {
		m.retagAnnotation(m.file.name, line, changeType, m.annot.kind)
		m.cancelAnnotation()
		return
	}
	m.saveComment(text, m.annot.kind, m.file.name, fileLevel, line, changeType)
}

// retagAnnotation sets the kind of the existing annotation at the target,
// keeping its comment and range. Returns false when no annotation exists there.
// File-level targets use line 0 and an empty changeType.
func (m *Model) retagAnnotation(fileName string, line int, changeType, kind string) bool {
	for _, a := range m.store.Get(fileName) {
		if a.Line != line || a.Type != changeType {
			continue
		}
		a.Kind = kind
		a.Status, a.Delivered = annotation.StatusOpen, false // an edit reopens and re-sends it
		if anchor := m.anchorFor(fileName, line, changeType); anchor != nil {
			a.Anchor = anchor
		}
		m.store.Add(a)
		m.saveSession()
		return true
	}
	return false
}

// anchorFor captures the re-anchoring snapshot for the line (line, changeType)
// of fileName when that file is the one loaded; nil otherwise and for
// file-level targets.
func (m Model) anchorFor(fileName string, line int, changeType string) *annotation.Anchor {
	if line <= 0 || fileName != m.file.name {
		return nil
	}
	return annotation.NewAnchor(m.file.lines, m.findDiffLineIndex(line, changeType))
}

// saveComment persists the annotation text for the explicitly provided target.
// Target fields are taken as arguments (not read from model state) so the
// Enter-key path and the editor-finished path can both use it without
// temporal coupling on cursor position or current file. Hunk-end detection for
// line-level saves re-derives the diffLines index from the (line, changeType)
// pair so cursor movement during an external editor session does not skew the
// range; when fileName matches the currently loaded file, m.file.lines is
// scanned, otherwise EndLine expansion is skipped (no hunk context available).
// An empty text is saved only when kind is set (a kind-only annotation such as
// a bare praise); empty text with no kind cancels.
func (m *Model) saveComment(text, kind, fileName string, fileLevel bool, line int, changeType string) {
	if text == "" && kind == "" {
		m.cancelAnnotation()
		return
	}

	if fileLevel {
		m.store.Add(annotation.Annotation{File: fileName, Line: 0, Type: "", Comment: text, Kind: kind})
		m.saveSession()
		m.annot.annotating = false
		m.annot.fileAnnotating = false
		m.annot.existingMultiline = ""
		m.annot.kind = ""
		m.nav.diffCursor = -1 // position cursor on the file annotation line
		m.tree.RefreshFilter(m.annotatedFiles())
		m.layout.viewport.SetContent(m.renderDiff())
		m.layout.viewport.GotoTop()
		return
	}

	a := annotation.Annotation{File: fileName, Line: line, Type: changeType, Comment: text, Kind: kind,
		Anchor: m.anchorFor(fileName, line, changeType)}
	if hunkKeywordRe.MatchString(text) && fileName == m.file.name {
		// re-derive the diff-line index from (line, changeType) so hunk-end
		// detection survives cursor drift during an external editor session.
		// only scan when the captured file still matches the loaded one —
		// otherwise m.file.lines describes a different file and would mislead.
		for i, dl := range m.file.lines {
			if string(dl.ChangeType) != changeType {
				continue
			}
			if m.diffLineNum(dl) != line {
				continue
			}
			if endLine := m.hunkEndLine(i); endLine > line {
				a.EndLine = endLine
			}
			break
		}
	}
	m.store.Add(a)
	m.saveSession()
	m.annot.annotating = false
	m.annot.fileAnnotating = false // defensive hygiene: parity with file-level branch
	m.annot.existingMultiline = ""
	m.annot.kind = ""
	m.tree.RefreshFilter(m.annotatedFiles())
	// sync scroll so a newly added multi-row annotation stays visible when the
	// cursor sits near the bottom of the viewport.
	m.syncViewportToCursor()
}

// handleAnnotationAction runs the diff-pane actions that change the annotation
// under the cursor: delete, quick praise, and resolve/reopen.
func (m Model) handleAnnotationAction(action keymap.Action) (tea.Model, tea.Cmd) {
	switch action {
	case keymap.ActionDeleteAnnotation:
		cmd := m.deleteAnnotation()
		return m, cmd
	case keymap.ActionQuickPraise:
		m.quickPraise()
	case keymap.ActionResolveAnnotation:
		m.toggleResolveAnnotation()
	default:
	}
	return m, nil
}

// toggleResolveAnnotation resolves the open annotation of the cursor line (or
// of the file-level annotation line), or reopens a resolved or outdated one.
// Resolving closes the comment: it is never output again. Reopening makes it
// open and undelivered, re-anchored at the line it is shown on, so it is sent
// with the next output.
func (m *Model) toggleResolveAnnotation() {
	line, changeType := 0, ""
	if !m.cursorOnFileAnnotationLine() {
		dl, ok := m.cursorDiffLine()
		if !ok || dl.ChangeType == diff.ChangeDivider {
			return
		}
		line, changeType = m.diffLineNum(dl), string(dl.ChangeType)
	}
	for _, a := range m.store.Get(m.file.name) {
		if a.Line != line || a.Type != changeType {
			continue
		}
		if a.Status == annotation.StatusOpen {
			a.Status = annotation.StatusResolved
			m.session.hint = "Annotation resolved — it will not be sent again; x reopens it"
		} else {
			a.Status, a.Delivered = annotation.StatusOpen, false
			if anchor := m.anchorFor(m.file.name, line, changeType); anchor != nil {
				a.Anchor = anchor
			}
			m.session.hint = "Annotation reopened — it will be sent with the next output"
		}
		m.store.Add(a)
		m.saveSession()
		m.syncViewportToCursor()
		return
	}
	m.session.hint = "No annotation on this line"
}

// quickPraise tags the cursor line, or the file-level annotation line, as
// praise without opening the input. An existing annotation keeps its comment
// and range and only gets the praise kind; otherwise a comment-less praise
// annotation is added. Line guards match startAnnotation, so dividers,
// collapsed-hidden lines and delete-only placeholders are ignored.
func (m *Model) quickPraise() {
	if m.cursorOnFileAnnotationLine() {
		m.retagAnnotation(m.file.name, 0, "", annotation.KindPraise)
		m.syncViewportToCursor()
		return
	}
	dl, ok := m.cursorDiffLine()
	if !ok || dl.ChangeType == diff.ChangeDivider {
		return
	}
	hunks := m.findHunks()
	if m.isCollapsedHidden(m.nav.diffCursor, hunks) || m.isDeleteOnlyPlaceholder(m.nav.diffCursor, hunks) {
		return
	}
	line, changeType := m.diffLineNum(dl), string(dl.ChangeType)
	if !m.retagAnnotation(m.file.name, line, changeType, annotation.KindPraise) {
		m.store.Add(annotation.Annotation{File: m.file.name, Line: line, Type: changeType, Kind: annotation.KindPraise,
			Anchor: annotation.NewAnchor(m.file.lines, m.nav.diffCursor)})
		m.saveSession()
		m.tree.RefreshFilter(m.annotatedFiles())
	}
	m.syncViewportToCursor()
}

// cancelAnnotation exits annotation input mode without saving.
func (m *Model) cancelAnnotation() {
	m.annot.annotating = false
	m.annot.fileAnnotating = false
	m.annot.existingMultiline = ""
	m.annot.kind = ""
	m.layout.viewport.SetContent(m.renderDiff())
}

// deleteFileAnnotation removes the file-level annotation and adjusts cursor position.
func (m *Model) deleteFileAnnotation() tea.Cmd {
	if !m.store.Delete(m.file.name, 0, "") {
		return nil
	}
	m.saveSession()
	m.pendingAnnotJump = nil    // clear before refreshFilter which may trigger file load
	m.nav.pendingHunkJump = nil // clear before refreshFilter which may trigger file load
	m.skipInitialDividers()

	m.tree.RefreshFilter(m.annotatedFiles())

	if newFile := m.tree.SelectedFile(); newFile != "" && newFile != m.file.name {
		return m.requestFileDiff(newFile)
	}

	m.syncViewportToCursor()
	return nil
}

// deleteAnnotation removes the annotation on the current cursor line if one exists.
// handles both file-level annotations (cursor at -1) and regular line annotations.
// only works when cursor is on the annotation sub-line (cursorOnAnnotation=true) or file annotation line.
// returns a command to load the new file if the tree selection changed after filter refresh.
func (m *Model) deleteAnnotation() tea.Cmd {
	if m.cursorOnFileAnnotationLine() {
		return m.deleteFileAnnotation()
	}

	if !m.annot.cursorOnAnnotation {
		return nil
	}

	dl, ok := m.cursorDiffLine()
	if !ok || dl.ChangeType == diff.ChangeDivider {
		return nil
	}

	lineNum := m.diffLineNum(dl)
	if m.store.Delete(m.file.name, lineNum, string(dl.ChangeType)) {
		m.saveSession()
		m.pendingAnnotJump = nil    // clear before refreshFilter which may trigger file load
		m.nav.pendingHunkJump = nil // clear before refreshFilter which may trigger file load
		m.annot.cursorOnAnnotation = false
		m.tree.RefreshFilter(m.annotatedFiles())

		// if filter moved cursor to a different file, load the new selection
		if newFile := m.tree.SelectedFile(); newFile != "" && newFile != m.file.name {
			return m.requestFileDiff(newFile)
		}

		m.syncViewportToCursor()
	}
	return nil
}

// editorKeyDisplay returns the display name for the open_editor binding
// (e.g. "Ctrl+E") for use in placeholder text. Returns empty when unbound.
// Filters out chord bindings since they don't fire during annotation input.
func (m Model) editorKeyDisplay() string {
	keys := m.keymap.KeysFor(keymap.ActionOpenEditor)
	var single []string
	for _, k := range keys {
		if strings.Index(k, ">") <= 0 {
			single = append(single, m.displayKeyName(k))
		}
	}
	return strings.Join(single, " / ")
}

func (m Model) multiLinePlaceholder() string {
	if key := m.editorKeyDisplay(); key != "" {
		return fmt.Sprintf("[existing multi-line — %s to edit]", key)
	}
	return "[existing multi-line]"
}

// kindBadge returns the "[kind] " badge painted before annotation text, or ""
// for a plain comment.
func (m Model) kindBadge(kind string) string {
	if kind == "" {
		return ""
	}
	return "[" + kind + "] "
}

// status badges painted before an annotation's kind badge. The outdated and
// resolved badges also dim the annotation's rows (see annotationDimmed).
const (
	badgeOutdated = "[outdated] "
	badgeResolved = "[resolved] "
	badgeSent     = "[sent] "
)

// annotationDisplayBody returns the text painted for a saved annotation: a
// status badge (outdated, resolved, or sent for a delivered open one), the
// kind badge, then the comment. Folding the badges into the body keeps them
// inside annotationVisualRows' cache key and the diff render cache's comment
// flag, so a status change repaints without any extra invalidation.
func (m Model) annotationDisplayBody(a annotation.Annotation) string {
	status := ""
	switch {
	case a.Status == annotation.StatusOutdated:
		status = badgeOutdated
	case a.Status == annotation.StatusResolved:
		status = badgeResolved
	case a.Delivered:
		status = badgeSent
	}
	if a.Comment == "" {
		return strings.TrimSuffix(status+m.kindBadge(a.Kind), " ")
	}
	return status + m.kindBadge(a.Kind) + a.Comment
}

// annotationDimmed reports whether a display body belongs to an outdated or
// resolved annotation, whose rows paint faint. It reads the badge
// annotationDisplayBody put in front, the only producer of that prefix.
func (m Model) annotationDimmed(body string) bool {
	return strings.HasPrefix(body, badgeOutdated) || strings.HasPrefix(body, badgeResolved) ||
		body == strings.TrimSuffix(badgeOutdated, " ") || body == strings.TrimSuffix(badgeResolved, " ")
}

// annotInputPrefix returns the prefix painted before the live annotation input:
// the line or file-level marker plus the selected kind badge.
func (m Model) annotInputPrefix() string {
	prefix := m.annotPrefix()
	if m.annot.fileAnnotating {
		prefix = m.annotFilePrefix()
	}
	return prefix + m.kindBadge(m.annot.kind)
}

// annotInputPrefixWidth is the width reserved before the textinput:
// cursor col + annotation prefix (with kind badge) + border margin.
func (m Model) annotInputPrefixWidth() int {
	return 3 + lipgloss.Width(m.annotInputPrefix())
}

// cycleAnnotationKind moves the selected kind of the open annotation input by
// step through "" (plain) followed by annotation.Kinds(), wrapping at both ends,
// and resizes the textinput so the badge does not push it past the pane edge.
func (m *Model) cycleAnnotationKind(step int) {
	order := append([]string{""}, annotation.Kinds()...)
	i := max(slices.Index(order, m.annot.kind), 0)
	m.annot.kind = order[((i+step)%len(order)+len(order))%len(order)]
	m.annot.input.Width = max(10, m.diffContentWidth()-m.annotInputPrefixWidth())
}

// handleAnnotateKey handles key messages during annotation input mode.
func (m Model) handleAnnotateKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyEnter:
		m.saveAnnotation()
		return m, nil
	case tea.KeyEsc:
		m.cancelAnnotation()
		return m, nil
	case tea.KeyTab, tea.KeyShiftTab:
		step := 1
		if msg.Type == tea.KeyShiftTab {
			step = -1
		}
		m.cycleAnnotationKind(step)
		m.layout.viewport.SetContent(m.renderDiff())
		return m, nil
	default:
		if m.keymap.Resolve(msg.String()) == keymap.ActionOpenEditor {
			cmd := m.openEditor()
			return m, cmd
		}
		var cmd tea.Cmd
		m.annot.input, cmd = m.annot.input.Update(msg)
		m.layout.viewport.SetContent(m.renderDiff()) // re-render so typed characters are visible immediately
		return m, cmd
	}
}

// cursorLineHasAnnotation checks if the cursor is on a deletable annotation line.
// returns true only when cursor is on the file annotation line or on an annotation sub-line.
func (m Model) cursorLineHasAnnotation() bool {
	return m.cursorOnFileAnnotationLine() || m.annot.cursorOnAnnotation
}

// hasFileAnnotation checks if the current file has a file-level annotation (Line=0).
func (m Model) hasFileAnnotation() bool {
	for _, a := range m.store.Get(m.file.name) {
		if a.Line == 0 {
			return true
		}
	}
	return false
}

// cursorOnFileAnnotationLine returns true if the diff cursor is on the file-level annotation line.
func (m Model) cursorOnFileAnnotationLine() bool {
	return m.nav.diffCursor == -1 && m.hasFileAnnotation()
}

// diffLineNum returns the display line number for a diff line.
func (m Model) diffLineNum(dl diff.DiffLine) int {
	if dl.ChangeType == diff.ChangeRemove {
		return dl.OldNum
	}
	return dl.NewNum
}

// hunkEndLine returns the display line number of the last line in the change hunk
// containing diffLines[idx]. only walks forward through lines of the same change type
// as the starting line, so both start and end use the same number space (old or new).
// returns 0 if idx is not inside a change hunk.
func (m Model) hunkEndLine(idx int) int {
	if idx < 0 || idx >= len(m.file.lines) {
		return 0
	}
	dl := m.file.lines[idx]
	if dl.ChangeType != diff.ChangeAdd && dl.ChangeType != diff.ChangeRemove {
		return 0
	}

	// walk forward from idx to find the last contiguous line of the same change type
	startType := dl.ChangeType
	last := idx
	for i := idx + 1; i < len(m.file.lines); i++ {
		if m.file.lines[i].ChangeType != startType {
			break
		}
		last = i
	}
	return m.diffLineNum(m.file.lines[last])
}

// annotCacheKey is the lookup key for annotationState.rowCache. fields are
// the comparable inputs to the wrap+style pipeline: cache hits require all
// three to match. width self-invalidates: a different pane width produces a
// different key and triggers a fresh compute. field order matches the
// (prefix, body, width) API convention used by annotationVisualRows and
// composeAnnotationRows.
type annotCacheKey struct {
	prefix, body string
	width        int
}

// annotationPrefixBody resolves the (prefix, body) pair for the annotation
// identified by key. file-level annotations (key == annotKeyFile) get the
// file-level prefix; line-level annotations get the line prefix. returns ("", "")
// when no annotation matches the key.
func (m Model) annotationPrefixBody(key string) (prefix, body string) {
	for _, a := range m.store.Get(m.file.name) {
		if key == annotKeyFile && a.Line == 0 {
			return m.annotFilePrefix(), m.annotationDisplayBody(a)
		}
		if key != annotKeyFile && m.annotationKey(a.Line, a.Type) == key {
			return m.annotPrefix(), m.annotationDisplayBody(a)
		}
	}
	return "", ""
}

// annotationVisualRows is the single source of truth for how an annotation is
// painted: it returns the fully-styled visual rows for (prefix, body) at the
// current pane width. wrappedAnnotationLineCount uses len() of this; the
// painter iterates these rows directly. results are memoized on rowCache;
// invalidation is the caller's responsibility (handleFileLoaded, applyTheme,
// cancelThemeSelect). pointer receiver is mandatory: the method writes to
// m.annot.rowCache and the consistency with invalidateRenderCaches protects
// against future LRU/slice replacements that would silently no-op on a value
// receiver.
func (m *Model) annotationVisualRows(prefix, body string) []string {
	// 1 for cursor column; clamp to wrapMinContent so the cache key normalizes
	// tiny-pane widths that all produce identical no-wrap output.
	wrapW := max(m.diffContentWidth()-1, wrapMinContent)
	key := annotCacheKey{prefix: prefix, body: body, width: wrapW}
	if rows, ok := m.annot.rowCache[key]; ok {
		return rows
	}
	rows := m.composeAnnotationRows(prefix, body, wrapW)
	m.annot.rowCache[key] = rows
	return rows
}

// composeAnnotationRows builds the styled visual rows for an annotation.
// splits body on "\n" into logical lines, applies prefix on row 0 and a matching-width
// plain-space indent on continuation rows so body columns line up, wraps each segment
// at wrapW, and wraps each visual row in AnnotationInline.
//
// IMPORTANT: the returned rows bake in the resolver's AnnotationInline styling
// envelope. applyTheme rebuilds the resolver — invalidation of rowCache on
// applyTheme is load-bearing. anyone adding a runtime color toggle that affects
// AnnotationInline MUST also invalidate the cache.
func (m Model) composeAnnotationRows(prefix, body string, wrapW int) []string {
	first := prefix + body
	logical := strings.Split(first, "\n")
	indent := strings.Repeat(" ", lipgloss.Width(prefix))

	dim := m.annotationDimmed(body)
	var rows []string
	for i, segment := range logical {
		if i > 0 {
			segment = indent + segment
		}
		var lines []string
		if wrapW > wrapMinContent && lipgloss.Width(segment) > wrapW {
			lines = m.wrapContent(segment, wrapW)
		} else {
			lines = []string{segment}
		}
		for _, line := range lines {
			row := m.renderer.AnnotationInline(line)
			if dim {
				row = "\033[2m" + row + "\033[22m" // faint on/off only, so pane and line backgrounds survive
			}
			rows = append(rows, row)
		}
	}
	return rows
}

// wrappedAnnotationLineCount returns the number of visual rows an annotation occupies.
// annotations always wrap at the pane width regardless of wrapMode.
// resolves (prefix, body) via annotationPrefixBody and defers to the chokepoint
// annotationVisualRows so the height query and the painter cannot drift apart.
// returns 1 when no annotation matches the key (preserves prior behavior). when an
// annotation exists with an empty body, the chokepoint produces a single prefix-only
// row, matching master's behavior for blank annotations preloaded via --annotations.
func (m *Model) wrappedAnnotationLineCount(key string) int {
	prefix, body := m.annotationPrefixBody(key)
	if prefix == "" && body == "" {
		return 1
	}
	return len(m.annotationVisualRows(prefix, body))
}

// hunkLineHeight returns the visual row count for a single diff line,
// including collapsed visibility, wrap, and inline annotation.
func (m Model) hunkLineHeight(idx int, hunks []int, annotationSet map[string]bool) int {
	if m.isCollapsedHidden(idx, hunks) {
		return 0
	}
	if m.isDeleteOnlyPlaceholder(idx, hunks) {
		return m.deletePlaceholderVisualHeight(idx)
	}
	h := m.wrappedLineCount(idx)
	dl := m.file.lines[idx]
	if dl.ChangeType != diff.ChangeDivider {
		key := m.annotationKey(m.diffLineNum(dl), string(dl.ChangeType))
		if annotationSet[key] {
			h += m.wrappedAnnotationLineCount(key)
		}
	}
	return h + len(m.inlineNoteRows(idx))
}

// cursorViewportY computes the actual viewport Y position of the cursor,
// accounting for injected annotation lines and the file-level annotation line.
// in collapsed mode, hidden removed lines (those in non-expanded hunks) are not counted.
func (m Model) cursorViewportY() int {
	var hunks []int
	if m.modes.collapsed.enabled {
		hunks = m.findHunks()
	}
	return m.cursorViewportYUsing(hunks, m.buildAnnotationSet())
}

// cursorViewportYUsing is the same as cursorViewportY but accepts pre-built
// hunks and annotationSet to avoid redundant computation when the caller
// already has them (e.g. centerHunkInViewport).
func (m Model) cursorViewportYUsing(hunks []int, annotationSet map[string]bool) int {
	if m.file.name == "" || len(m.file.lines) == 0 {
		return max(0, m.nav.diffCursor)
	}

	if m.nav.diffCursor == -1 {
		return 0
	}

	y := m.diffHeaderRows()
	for i := 0; i < m.nav.diffCursor && i < len(m.file.lines); i++ {
		y += m.hunkLineHeight(i, hunks, annotationSet)
	}
	if m.annot.cursorOnAnnotation {
		y += m.wrappedLineCount(m.nav.diffCursor)
	}
	return y
}

// cursorVisualOffsets returns the top visual row for every diff line. Page
// motions build this index once so each cursor step can read its visual
// position in O(1) instead of rescanning all preceding lines.
func (m Model) cursorVisualOffsets(hunks []int, annotationSet map[string]bool) []int {
	offsets := make([]int, len(m.file.lines))
	y := m.diffHeaderRows()
	for i := range m.file.lines {
		offsets[i] = y
		y += m.hunkLineHeight(i, hunks, annotationSet)
	}
	return offsets
}

// cursorViewportYFromOffsets returns the current cursor's visual row from a
// pre-built line-offset index. offsets must come from cursorVisualOffsets for
// the current m.file.lines, and m.nav.diffCursor must be in [-1, len(offsets)).
func (m Model) cursorViewportYFromOffsets(offsets []int) int {
	if m.file.name == "" || len(offsets) == 0 {
		return max(0, m.nav.diffCursor)
	}
	if m.nav.diffCursor == -1 {
		return 0
	}
	y := offsets[m.nav.diffCursor]
	if m.annot.cursorOnAnnotation {
		y += m.wrappedLineCount(m.nav.diffCursor)
	}
	return y
}

// cursorVisualRange returns the top and bottom viewport Y coordinates the
// cursor currently occupies. when the cursor is on a diff row, bottom spans
// any wrap-continuation rows plus any injected annotation rows below it;
// when the cursor is on an annotation sub-line, bottom spans only the
// annotation rows (the diff row sits above top). callers keeping the cursor
// "visible" use this range to preserve the full logical extent, not just the
// top row.
func (m Model) cursorVisualRange() (top, bottom int) {
	var hunks []int
	if m.modes.collapsed.enabled {
		hunks = m.findHunks()
	}
	annotationSet := m.buildAnnotationSet()
	top = m.cursorViewportYUsing(hunks, annotationSet)
	h := max(m.cursorVisualHeight(hunks, annotationSet), 1)
	return top, top + h - 1
}

// rowOnAnnotationSubLine reports whether relRow (0-based, relative to the first
// visual row of the diff line at idx) targets the injected annotation sub-line
// below that diff line. h is the total visual height of the diff line (wrap rows
// plus any injected annotation rows). delete-only placeholders always return
// false because renderCollapsedDiff skips annotation rendering for them, even
// when the underlying removed line has an annotation.
func (m Model) rowOnAnnotationSubLine(idx, relRow, h int, hunks []int, annSet map[string]bool) bool {
	if m.isDeleteOnlyPlaceholder(idx, hunks) {
		return false
	}
	dl := m.file.lines[idx]
	if dl.ChangeType == diff.ChangeDivider {
		return false
	}
	key := m.annotationKey(m.diffLineNum(dl), string(dl.ChangeType))
	if !annSet[key] {
		return false
	}
	// the annotation rows sit between the diff line and any inline note rows
	annRows := m.wrappedAnnotationLineCount(key)
	noteRows := len(m.inlineNoteRows(idx))
	return annRows > 0 && relRow >= h-noteRows-annRows && relRow < h-noteRows
}

// visualRowToDiffLine maps a visual row within the diff viewport content back
// to a diff-line index. row is 0-based relative to the first visible content
// row of the viewport (caller must add YOffset and subtract any header rows).
// when the file has a file-level annotation, rows covered by that annotation
// map to idx=-1. the onAnnotation return value mirrors the semantics of
// m.annot.cursorOnAnnotation: true when the row falls on an injected
// annotation sub-line rather than the diff line (or its wrap-continuation)
// itself. this is the inverse of cursorVisualRange + cursorViewportYUsing.
//
// edge cases: empty m.file.lines returns (m.nav.diffCursor, false); row < 0
// returns (-1, false) when a file-level annotation is present, else
// (firstVisibleIdx, false); rows past the last line return the last valid
// index.
func (m Model) visualRowToDiffLine(row int) (idx int, onAnnotation bool) {
	if len(m.file.lines) == 0 {
		if m.hasFileAnnotation() && row >= 0 && row < m.wrappedAnnotationLineCount(annotKeyFile) {
			return -1, false
		}
		return m.nav.diffCursor, false
	}

	var hunks []int
	if m.modes.collapsed.enabled {
		hunks = m.findHunks()
	}
	annSet := m.buildAnnotationSet()

	if m.hasFileAnnotation() && row < m.wrappedAnnotationLineCount(annotKeyFile) {
		return -1, false
	}
	running := m.diffHeaderRows()
	if row < running {
		// above the first line (no file annotation) or on the inline overview:
		// pick the first visible line
		for i := range m.file.lines {
			if m.hunkLineHeight(i, hunks, annSet) > 0 {
				return i, false
			}
		}
		return 0, false
	}

	for i := range m.file.lines {
		h := m.hunkLineHeight(i, hunks, annSet)
		if h == 0 {
			continue
		}
		if row < running+h {
			return i, m.rowOnAnnotationSubLine(i, row-running, h, hunks, annSet)
		}
		running += h
	}
	// row past the last visible line: return the last visible line index
	for i := range slices.Backward(m.file.lines) {
		if m.hunkLineHeight(i, hunks, annSet) > 0 {
			return i, false
		}
	}
	return len(m.file.lines) - 1, false
}

// cursorVisualHeight returns the number of visual rows occupied by the cursor.
// branches, in order of evaluation:
//   - cursor on the file-level annotation line → file annotation's wrapped row count
//   - diffCursor out of range (empty or not loaded) → 1
//   - cursor on annotation sub-line of a divider (defensive; unreachable today) → 1
//   - cursor on annotation sub-line of a regular line → annotation's own wrapped row count
//   - cursor on the diff row → hunkLineHeight (wrap + injected annotation rows)
//
// hunks and annotationSet are supplied by the caller to avoid redundant
// computation when they are already built; both must describe the current file.
func (m Model) cursorVisualHeight(hunks []int, annotationSet map[string]bool) int {
	if m.cursorOnFileAnnotationLine() {
		return m.wrappedAnnotationLineCount(annotKeyFile)
	}
	if m.nav.diffCursor < 0 || m.nav.diffCursor >= len(m.file.lines) {
		return 1
	}
	dl := m.file.lines[m.nav.diffCursor]
	if m.annot.cursorOnAnnotation {
		if dl.ChangeType == diff.ChangeDivider {
			return 1
		}
		return m.wrappedAnnotationLineCount(m.annotationKey(m.diffLineNum(dl), string(dl.ChangeType)))
	}
	return m.hunkLineHeight(m.nav.diffCursor, hunks, annotationSet)
}

// buildAnnotationSet returns a set of annotation keys for the current file.
// excludes file-level annotations (Line=0) since they are rendered separately.
func (m Model) buildAnnotationSet() map[string]bool {
	annotations := m.store.Get(m.file.name)
	set := make(map[string]bool, len(annotations))
	for _, a := range annotations {
		if a.Line == 0 {
			continue
		}
		set[m.annotationKey(a.Line, a.Type)] = true
	}
	return set
}

// annotationKey creates a lookup key from line number and change type.
func (m Model) annotationKey(line int, changeType string) string {
	return fmt.Sprintf("%d:%s", line, changeType)
}
