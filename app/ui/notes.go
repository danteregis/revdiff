package ui

import (
	"fmt"
	"log"
	"slices"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"

	"github.com/umputun/revdiff/app/diff"
	"github.com/umputun/revdiff/app/keymap"
	"github.com/umputun/revdiff/app/notes"
)

// NotesStore reads and answers Claude's notes on a branch: Stamp and Load back
// the live poll, Reply records the reviewer's reply (thread + outbox),
// ListenerAlive tells whether an agent waits for replies, and MarkViewing
// tells the agent a revdiff shows the notes. Implemented by *notes.Repo. Wired
// through ModelConfig.Notes only together with review sessions; nil disables
// the feature and leaves the UI byte-identical.
type NotesStore interface {
	Stamp(branch string) notes.Stamp
	Load(branch string) (*notes.Document, error)
	Reply(branch, id, body string) (*notes.Document, error)
	ListenerAlive(branch string) bool
	MarkViewing(branch string) error
	UnmarkViewing(branch string)
}

// notesPollInterval is how often the notes file is checked for changes while
// notes are enabled. A poll stats one file; only a changed stamp reads it.
const notesPollInterval = time.Second

// notesState holds Claude's notes for the reviewed branch and the UI state
// around them. doc is the last loaded document; at/keys/lost describe the
// current file's notes located against the loaded diff and are rebuilt by
// locateNotes whenever the document or the file changes.
type notesState struct {
	store  NotesStore
	branch string // branch the loaded document belongs to
	doc    *notes.Document
	stamp  notes.Stamp
	loaded bool // a poll for branch completed (doc may still be nil)

	listener          bool // an agent runs `revdiff inbox --wait`
	paneHidden        bool // the notes pane was toggled off with `>`
	overviewCollapsed bool // the pinned overview is folded with `-`

	selected    string // note chosen by `)` / `(`, a reply or `c`
	selectedAt  int    // diff cursor when selected was set; moving the cursor releases it
	pendingJump string // note to put the cursor on when its file finishes loading

	at   map[int][]int  // current file: diff line index -> indexes into file
	keys map[int]string // current file: diff line index -> render key of its notes
	file []notes.Note   // current file's notes
	lost []int          // indexes of current-file line notes not found in the diff

	rowCache map[noteRowsKey][]string // inline note blocks by content and width
	scroll   int                      // notes pane scroll offset

	reply replyState
	hint  string // transient status-bar message; cleared on next key press
}

// replyState is the reply input shown in the status bar.
type replyState struct {
	active bool
	noteID string
	input  textinput.Model
}

// notesPolledMsg carries one poll of the notes file. changed is true when the
// file's stamp differs from the last loaded one (or the branch changed), in
// which case doc holds the freshly loaded document.
type notesPolledMsg struct {
	branch   string
	stamp    notes.Stamp
	changed  bool
	doc      *notes.Document
	err      error
	listener bool
}

// replyEditorFinishedMsg returns the reply written in $EDITOR. The note id is
// captured at launch so moving the cursor meanwhile cannot misroute it.
type replyEditorFinishedMsg struct {
	noteID       string
	content      string
	err          error
	restoreMouse bool
}

// notesBranch is the branch whose notes are shown: the active session's.
func (m Model) notesBranch() string {
	if m.notes.store == nil || !m.sessionsActive() {
		return ""
	}
	return m.session.cur.Branch
}

// pollNotes schedules the next notes poll after delay. The poll runs off the
// update loop: it refreshes the viewer heartbeat, stats the notes file and
// reads it only when its stamp changed (or the branch did).
func (m Model) pollNotes(delay time.Duration) tea.Cmd {
	if m.notes.store == nil {
		return nil
	}
	store, branch := m.notes.store, m.notesBranch()
	last, lastBranch, loaded := m.notes.stamp, m.notes.branch, m.notes.loaded
	return tea.Tick(delay, func(time.Time) tea.Msg {
		msg := notesPolledMsg{branch: branch}
		if branch == "" {
			return msg
		}
		if err := store.MarkViewing(branch); err != nil {
			log.Printf("[WARN] notes: mark viewing: %v", err)
		}
		msg.listener = store.ListenerAlive(branch)
		msg.stamp = store.Stamp(branch)
		if loaded && branch == lastBranch && msg.stamp == last {
			return msg
		}
		msg.changed = true
		msg.doc, msg.err = store.Load(branch)
		return msg
	})
}

// handleNotesMsg routes the notes messages.
func (m Model) handleNotesMsg(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case notesPolledMsg:
		return m.handleNotesPolled(msg)
	case replyEditorFinishedMsg:
		return m.handleReplyEditorFinished(msg)
	default:
		return m, nil
	}
}

// handleNotesPolled applies a poll result and schedules the next poll. A
// changed document is re-located against the current file without moving the
// cursor; the layout is refreshed because the notes pane may appear or go.
func (m Model) handleNotesPolled(msg notesPolledMsg) (tea.Model, tea.Cmd) {
	next := m.pollNotes(notesPollInterval)
	if msg.branch != m.notesBranch() {
		// the review switched branches while the poll ran: drop the result, and stop
		// showing the previous branch's notes until the next poll loads the new one's
		if m.notes.doc != nil && m.notes.branch != m.notesBranch() {
			m.notes.loaded = false
			m.setNotesDoc(nil)
		}
		return m, next
	}
	m.notes.listener = msg.listener
	if !msg.changed {
		return m, next
	}
	if msg.err != nil {
		log.Printf("[WARN] notes: %v", msg.err)
		if !m.notes.loaded || m.notes.branch != msg.branch {
			m.notes.hint = "Claude's notes could not be read: " + m.oneLine(msg.err.Error())
		}
		m.notes.stamp, m.notes.branch, m.notes.loaded = msg.stamp, msg.branch, true
		return m, next
	}
	if m.notes.branch != msg.branch && m.notes.branch != "" {
		m.notes.store.UnmarkViewing(m.notes.branch)
		m.notes.selected, m.notes.pendingJump = "", ""
	}
	m.notes.stamp, m.notes.branch, m.notes.loaded = msg.stamp, msg.branch, true
	m.setNotesDoc(msg.doc)
	return m, next
}

// setNotesDoc installs a document and refreshes everything derived from it.
func (m *Model) setNotesDoc(doc *notes.Document) {
	m.notes.doc = doc
	m.locateNotes()
	m.refreshLayoutWidths()
	if m.file.name != "" && m.ready {
		m.syncViewportToCursor()
	}
}

// notesActive reports whether the review has notes to show.
func (m Model) notesActive() bool {
	return m.notes.store != nil && m.notes.doc.Count() > 0
}

// notesInline reports whether notes render inside the diff (pane hidden or
// too narrow) rather than in the notes pane.
func (m Model) notesInline() bool {
	return m.notesActive() && !m.notesPaneVisible()
}

// notesPaneWidth is the inner width of the notes pane: 30% of the terminal,
// clamped to [notesPaneMinWidth, notesPaneMaxWidth].
func (m Model) notesPaneWidth() int {
	return min(max(m.layout.width*3/10, notesPaneMinWidth), notesPaneMaxWidth)
}

// notes pane geometry. The pane is shown only when the diff pane keeps at
// least notesMinDiffWidth columns beside it; below that notes render inline.
const (
	notesPaneMinWidth = 34
	notesPaneMaxWidth = 60
	notesMinDiffWidth = 60
)

// notesPaneVisible reports whether the notes pane is on screen.
func (m Model) notesPaneVisible() bool {
	if !m.notesActive() || m.notes.paneHidden {
		return false
	}
	return m.baseDiffPaneWidth()-(m.notesPaneWidth()+2) >= notesMinDiffWidth
}

// notesPaneCols is the number of terminal columns the notes pane takes,
// borders included; 0 when it is not shown.
func (m Model) notesPaneCols() int {
	if !m.notesPaneVisible() {
		return 0
	}
	return m.notesPaneWidth() + 2
}

// baseDiffPaneWidth is the diff pane's inner width without the notes pane.
func (m Model) baseDiffPaneWidth() int {
	if m.treePaneHidden() {
		return m.layout.width - 2
	}
	return m.layout.width - m.layout.treeWidth - 4
}

// diffPaneWidth is the diff pane's inner width: what is left of the
// terminal after the tree/TOC pane and the notes pane, minus its borders.
func (m Model) diffPaneWidth() int {
	return m.baseDiffPaneWidth() - m.notesPaneCols()
}

// refreshLayoutWidths re-applies the diff viewport width after something that
// changes the notes pane's presence (a notes load, `>`).
func (m *Model) refreshLayoutWidths() {
	if !m.ready {
		return
	}
	m.layout.viewport.Width = m.diffPaneWidth()
}

// locateNotes finds the current file's line notes in its loaded diff. A note
// that cannot be found is kept in lost: listed in the notes pane as outdated,
// never drawn on a line.
func (m *Model) locateNotes() {
	m.notes.at, m.notes.keys, m.notes.lost, m.notes.file = nil, nil, nil, nil
	clear(m.notes.rowCache)
	if m.notes.store == nil || m.file.name == "" {
		return
	}
	m.notes.file = m.notes.doc.FileNotes(m.file.name)
	for i, n := range m.notes.file {
		if n.Kind == notes.KindOverview {
			continue
		}
		idx := n.Locate(m.file.lines)
		if idx < 0 {
			m.notes.lost = append(m.notes.lost, i)
			continue
		}
		if m.notes.at == nil {
			m.notes.at = map[int][]int{}
			m.notes.keys = map[int]string{}
		}
		m.notes.at[idx] = append(m.notes.at[idx], i)
	}
	for idx, list := range m.notes.at {
		var b strings.Builder
		for _, i := range list {
			b.WriteString(m.noteRenderKey(m.notes.file[i]))
		}
		m.notes.keys[idx] = b.String()
	}
}

// noteRenderKey captures everything an inline note block paints, so equal
// keys render equal rows.
func (m Model) noteRenderKey(n notes.Note) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s\x00%s\x00%s\x00", n.ID, n.Kind, n.Body)
	for _, t := range n.Thread {
		fmt.Fprintf(&b, "%s\x00%s\x00%s\x00", t.Author, t.State, t.Body)
	}
	b.WriteString("\x01")
	return b.String()
}

// fileOverview returns the current file's overview note, or nil.
func (m Model) fileOverview() *notes.Note {
	for i := range m.notes.file {
		if m.notes.file[i].Kind == notes.KindOverview {
			return &m.notes.file[i]
		}
	}
	return nil
}

// noteIndex returns the index of note id among the current file's notes, or -1.
func (m Model) noteIndex(id string) int {
	for i, n := range m.notes.file {
		if n.ID == id {
			return i
		}
	}
	return -1
}

// noteLine returns the diff line index the current file's note i is drawn on, or -1.
func (m Model) noteLine(i int) int {
	for idx, list := range m.notes.at {
		if slices.Contains(list, i) {
			return idx
		}
	}
	return -1
}

// currentNote returns the index (into the current file's notes) of the note
// the reviewer is looking at: the one chosen by navigation while the cursor
// has not moved since, else a note on the cursor line, else the nearest note
// above the cursor, else the nearest below, else the overview. -1 when the
// file has no notes.
func (m Model) currentNote() int {
	if len(m.notes.file) == 0 {
		return -1
	}
	if i := m.noteAtCursor(); i >= 0 {
		return i
	}
	above, below := -1, -1
	for idx := range m.notes.at {
		if idx < m.nav.diffCursor && idx > above {
			above = idx
		}
		if idx > m.nav.diffCursor && (below < 0 || idx < below) {
			below = idx
		}
	}
	switch {
	case above >= 0:
		list := m.notes.at[above]
		return list[len(list)-1]
	case below >= 0:
		return m.notes.at[below][0]
	}
	for i, n := range m.notes.file {
		if n.Kind == notes.KindOverview {
			return i
		}
	}
	return 0
}

// handleNotesAction runs the notes key actions.
func (m Model) handleNotesAction(action keymap.Action) (tea.Model, tea.Cmd) {
	if !m.notesActive() {
		m.notes.hint = "No notes from Claude for this review"
		return m, nil
	}
	switch action {
	case keymap.ActionToggleNotes:
		m.toggleNotesPane()
	case keymap.ActionToggleOverview:
		m.notes.overviewCollapsed = !m.notes.overviewCollapsed
		m.invalidateNoteRows()
		m.syncViewportToCursor()
	case keymap.ActionReplyNote:
		cmd := m.startReply()
		return m, cmd
	case keymap.ActionNoteToAnnotation:
		cmd := m.noteToAnnotation()
		return m, cmd
	case keymap.ActionNextNote, keymap.ActionPrevNote:
		return m.navigateNotes(action == keymap.ActionNextNote)
	default:
	}
	return m, nil
}

// toggleNotesPane shows or hides the notes pane. Hidden or too narrow, notes
// render inline under their lines instead.
func (m *Model) toggleNotesPane() {
	m.notes.paneHidden = !m.notes.paneHidden
	if !m.notes.paneHidden && !m.notesPaneVisible() {
		m.notes.hint = "Too narrow for the notes pane — notes are shown inline"
	}
	m.refreshLayoutWidths()
	m.invalidateNoteRows()
	if m.file.name != "" {
		m.syncViewportToCursor()
	}
}

// invalidateNoteRows drops the memoized inline note blocks (theme or collapse
// changes; width self-invalidates through the key).
func (m *Model) invalidateNoteRows() {
	clear(m.notes.rowCache)
}

// startReply opens the reply input for the current note.
func (m *Model) startReply() tea.Cmd {
	i := m.currentNote()
	if i < 0 {
		m.notes.hint = "No note in this file to reply to — use ) to go to the next note"
		return nil
	}
	m.clearPendingInputState()
	n := m.notes.file[i]
	ti := textinput.New()
	ti.Prompt = ""
	ti.Placeholder = "reply to Claude..."
	if key := m.editorKeyDisplay(); key != "" {
		ti.Placeholder = fmt.Sprintf("reply to Claude... (%s for editor)", key)
	}
	ti.CharLimit = annotCharLimit
	ti.Width = max(10, m.layout.width-len(m.replyPrompt(n.ID))-4)
	cmd := ti.Focus()
	m.notes.reply = replyState{active: true, noteID: n.ID, input: ti}
	m.notes.selected, m.notes.selectedAt = n.ID, m.nav.diffCursor
	if m.cfg.noStatusBar {
		// the input lives in the status bar; without one the reply is written in $EDITOR
		return m.openReplyEditor()
	}
	return cmd
}

// replyPrompt is the status-bar prompt of the reply input.
func (m Model) replyPrompt(id string) string {
	return "◆ reply to " + id + ": "
}

// handleReplyKey drives the reply input: enter sends, esc cancels, the
// open_editor key moves the draft to $EDITOR.
func (m Model) handleReplyKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	switch msg.Type {
	case tea.KeyEnter:
		body := m.notes.reply.input.Value()
		id := m.notes.reply.noteID
		m.notes.reply = replyState{}
		m.sendReply(id, body)
		return m, nil
	case tea.KeyEsc:
		m.notes.reply = replyState{}
		m.notes.hint = "Reply canceled"
		return m, nil
	default:
	}
	if m.keymap.Resolve(msg.String()) == keymap.ActionOpenEditor {
		cmd := m.openReplyEditor()
		return m, cmd
	}
	var cmd tea.Cmd
	m.notes.reply.input, cmd = m.notes.reply.input.Update(msg)
	return m, cmd
}

// sendReply records the reviewer's reply to note id and shows it at once.
func (m *Model) sendReply(id, body string) {
	if strings.TrimSpace(body) == "" {
		m.notes.hint = "Reply canceled"
		return
	}
	branch := m.notesBranch()
	if branch == "" {
		m.notes.hint = "Notes are not available"
		return
	}
	doc, err := m.notes.store.Reply(branch, id, body)
	if err != nil {
		log.Printf("[WARN] notes: reply to %s: %v", id, err)
		m.notes.hint = "Reply failed: " + m.oneLine(err.Error())
		return
	}
	m.notes.stamp = m.notes.store.Stamp(branch)
	m.notes.selected, m.notes.selectedAt = id, m.nav.diffCursor
	m.setNotesDoc(doc)
	if m.notes.listener {
		m.notes.hint = "Reply sent to Claude"
		return
	}
	m.notes.hint = "Reply saved — no agent is listening (Claude reads it with revdiff inbox)"
}

// openReplyEditor moves the reply draft to $EDITOR.
func (m *Model) openReplyEditor() tea.Cmd {
	id, seed := m.notes.reply.noteID, m.notes.reply.input.Value()
	cmd, complete, err := m.editor.Command(seed)
	if err != nil {
		return func() tea.Msg { return replyEditorFinishedMsg{noteID: id, content: seed, err: err} }
	}
	restore := m.cfg.mouseTracking
	m.notes.reply = replyState{}
	return tea.ExecProcess(cmd, func(runErr error) tea.Msg {
		text, finalErr := complete(runErr)
		return replyEditorFinishedMsg{noteID: id, content: text, err: finalErr, restoreMouse: restore}
	})
}

// handleReplyEditorFinished sends the reply written in $EDITOR. A failed
// editor keeps nothing but a hint; the draft text, if any came back, is sent.
func (m Model) handleReplyEditorFinished(msg replyEditorFinishedMsg) (tea.Model, tea.Cmd) {
	var cmd tea.Cmd
	if msg.restoreMouse {
		cmd = tea.EnableMouseCellMotion
	}
	if msg.err != nil {
		log.Printf("[WARN] notes: reply editor: %v", msg.err)
		if strings.TrimSpace(msg.content) == "" {
			m.notes.hint = "Reply editor failed: " + m.oneLine(msg.err.Error())
			return m, cmd
		}
	}
	m.sendReply(msg.noteID, msg.content)
	return m, cmd
}

// noteToAnnotation turns the current note into one of the reviewer's
// annotations: the annotation input opens on the note's line (file-level for
// an overview) with kind suggestion and the note quoted, so the reviewer adds
// what to change and the request travels the normal annotation path. A line
// that already carries an annotation opens it for editing unchanged.
func (m *Model) noteToAnnotation() tea.Cmd {
	i := m.currentNote()
	if i < 0 {
		m.notes.hint = "No note in this file — use ) to go to the next note"
		return nil
	}
	n := m.notes.file[i]
	quote := m.noteQuote(n)
	if n.Kind == notes.KindOverview {
		m.layout.focus = paneDiff
		cmd := m.startFileAnnotation()
		m.prefillSuggestion(quote)
		m.layout.viewport.SetContent(m.renderDiff())
		return cmd
	}
	idx := m.noteLine(i)
	if idx < 0 {
		m.notes.hint = "Note " + n.ID + " is outdated — its line is not in the diff"
		return nil
	}
	m.nav.diffCursor = idx
	m.annot.cursorOnAnnotation = false
	m.layout.focus = paneDiff
	m.ensureHunkExpanded(idx)
	m.notes.selected, m.notes.selectedAt = n.ID, idx
	cmd := m.startAnnotation()
	if !m.annot.annotating {
		m.notes.hint = "This line cannot be annotated"
		return nil
	}
	m.prefillSuggestion(quote)
	m.syncViewportToCursor()
	m.ensureLineAnnotationInputVisible()
	return cmd
}

// prefillSuggestion seeds a freshly opened, empty annotation input with the
// quoted note and kind suggestion. An existing annotation is left as it is.
func (m *Model) prefillSuggestion(quote string) {
	if !m.annot.annotating || m.annot.input.Value() != "" || m.annot.existingMultiline != "" {
		return
	}
	m.annot.kind = noteSuggestionKind
	m.annot.input.SetValue(quote)
	m.annot.input.CursorEnd()
}

// noteQuote renders a note as a one-line quote for an annotation.
func (m Model) noteQuote(n notes.Note) string {
	body := strings.Join(strings.Fields(diff.SanitizeCommitText(n.Body)), " ")
	const maxQuote = 200
	if r := []rune(body); len(r) > maxQuote {
		body = string(r[:maxQuote-1]) + "…"
	}
	return "re Claude's note “" + body + "”: "
}

// noteTarget is one stop of the `)` / `(` walk.
type noteTarget struct {
	file string
	id   string
}

// noteTour flattens the document into reading order: files in tour order
// (see notes.Document.TourOrder), each file's overview first, then its line
// notes by line.
func (m Model) noteTour() []noteTarget {
	var out []noteTarget
	for _, path := range m.notes.doc.TourOrder() {
		ns := slices.Clone(m.notes.doc.FileNotes(path))
		slices.SortStableFunc(ns, func(a, b notes.Note) int {
			ao, bo := a.Kind == notes.KindOverview, b.Kind == notes.KindOverview
			switch {
			case ao && !bo:
				return -1
			case bo && !ao:
				return 1
			}
			return a.Line - b.Line
		})
		for _, n := range ns {
			out = append(out, noteTarget{file: path, id: n.ID})
		}
	}
	return out
}

// navigateNotes moves to the next or previous note in reading order, across
// files. A file that cannot be shown (filtered out of the tree) is skipped.
func (m Model) navigateNotes(forward bool) (tea.Model, tea.Cmd) {
	tour := m.noteTour()
	if len(tour) == 0 {
		return m, nil
	}
	pos := m.tourPosition(tour, forward)
	step := 1
	if !forward {
		step = -1
	}
	for range tour {
		pos = (pos + step + len(tour)) % len(tour)
		t := tour[pos]
		if t.file == m.file.name {
			m.positionOnNote(t.id)
			return m, nil
		}
		if m.file.singleFile || !m.tree.SelectByPath(t.file) {
			continue
		}
		m.notes.pendingJump = t.id
		m.pendingAnnotJump = nil
		m.nav.pendingHunkJump = nil
		return m.loadSelectedIfChanged()
	}
	m.notes.hint = "No other note can be shown"
	return m, nil
}

// tourPosition returns the index in tour the walk steps from. On a note it
// is that note; elsewhere in a file with notes it is the gap at the cursor,
// so the walk reaches the first note after (or before) the cursor; in a file
// without notes the walk starts from the top (forward) or the end (backward).
func (m Model) tourPosition(tour []noteTarget, forward bool) int {
	if i := m.noteAtCursor(); i >= 0 {
		id := m.notes.file[i].ID
		for p, t := range tour {
			if t.file == m.file.name && t.id == id {
				return p
			}
		}
	}
	first, last := -1, -1
	for p, t := range tour {
		if t.file == m.file.name {
			if first < 0 {
				first = p
			}
			last = p
		}
	}
	switch {
	case first < 0 && forward:
		return -1
	case first < 0:
		return len(tour)
	case forward:
		for p := first; p <= last; p++ {
			if m.noteOrder(tour[p].id) > m.nav.diffCursor {
				return p - 1
			}
		}
		return last
	default:
		for p := last; p >= first; p-- {
			if m.noteOrder(tour[p].id) < m.nav.diffCursor {
				return p + 1
			}
		}
		return first
	}
}

// noteAtCursor returns the note the cursor is on: the one chosen by navigation
// while the cursor has not moved since, else the first note of the cursor
// line; -1 when there is none.
func (m Model) noteAtCursor() int {
	if m.notes.selected != "" && m.notes.selectedAt == m.nav.diffCursor {
		if i := m.noteIndex(m.notes.selected); i >= 0 {
			return i
		}
	}
	if list := m.notes.at[m.nav.diffCursor]; len(list) > 0 {
		return list[0]
	}
	return -1
}

// noteOrder places a current-file note on the diff for the walk: its line
// index, before every line for the overview, after every line when it could
// not be placed.
func (m Model) noteOrder(id string) int {
	i := m.noteIndex(id)
	switch {
	case i < 0:
		return len(m.file.lines)
	case m.notes.file[i].Kind == notes.KindOverview:
		return -2
	}
	if idx := m.noteLine(i); idx >= 0 {
		return idx
	}
	return len(m.file.lines)
}

// positionOnNote puts the cursor on note id of the current file: on its line
// for a line note, at the top (with the overview unfolded) for an overview.
// An outdated note is selected for the notes pane without moving the cursor.
func (m *Model) positionOnNote(id string) {
	i := m.noteIndex(id)
	if i < 0 {
		return
	}
	m.notes.scroll = 0
	n := m.notes.file[i]
	m.layout.focus = paneDiff
	switch {
	case n.Kind == notes.KindOverview:
		m.notes.overviewCollapsed = false
		m.invalidateNoteRows()
		m.moveDiffCursorToStart()
	case m.noteLine(i) >= 0:
		idx := m.noteLine(i)
		m.nav.diffCursor = idx
		m.annot.cursorOnAnnotation = false
		m.ensureHunkExpanded(idx)
		m.syncTOCActiveSection()
		m.centerViewportOnCursor()
	default:
		m.notes.hint = "Note " + id + " is outdated — its line is not in the diff"
	}
	m.notes.selected, m.notes.selectedAt = id, m.nav.diffCursor
}

// applyPendingNoteJump positions the cursor on the note a cross-file `)` /
// `(` walk was heading to, once its file has loaded. Reports whether it did.
func (m *Model) applyPendingNoteJump(file string) bool {
	id := m.notes.pendingJump
	if id == "" {
		return false
	}
	m.notes.pendingJump = ""
	if _, path := m.notes.doc.Find(id); path != file {
		return false
	}
	m.positionOnNote(id)
	return true
}

// noteCounts returns the number of notes per file for the tree, or nil when
// there are none (keeping the tree byte-identical).
func (m Model) noteCounts() map[string]int {
	if !m.notesActive() {
		return nil
	}
	out := map[string]int{}
	for _, f := range m.notes.doc.Files {
		if len(f.Notes) > 0 {
			out[f.Path] = len(f.Notes)
		}
	}
	return out
}

// notesStatusParts returns the status-bar segments for notes: the note count,
// pending replies and, when replies wait with nobody listening, a hint that no
// agent will see them yet.
func (m Model) notesStatusParts() []string {
	if !m.notesActive() {
		return nil
	}
	parts := []string{fmt.Sprintf("◆ %d", m.notes.doc.Count())}
	if pending := m.notes.doc.PendingReplies(); pending > 0 {
		parts = append(parts, m.plural(pending, "reply pending", "replies pending"))
		if !m.notes.listener {
			parts = append(parts, "no agent listening")
		}
	}
	return parts
}
