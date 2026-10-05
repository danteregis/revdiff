package overlay

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/mattn/go-runewidth"

	"github.com/umputun/revdiff/app/keymap"
	"github.com/umputun/revdiff/app/ui/style"
)

const (
	sessionsMaxWidth    = 90
	sessionsMinWidth    = 30
	sessionsMargin      = 10
	sessionsBorderPad   = 4
	sessionsChromeLines = 10
	sessionsNewLabel    = "+ New session"
)

// SessionsSpec describes the review-sessions picker: the sessions of the
// current branch plus a leading "new session" row.
type SessionsSpec struct {
	Branch   string        // branch the sessions belong to, shown in the title
	ActiveID string        // session currently in use, marked and preselected
	Items    []SessionItem // newest first
	Notice   string        // muted message under the list (e.g. a listing error or a refused action)
}

// SessionItem is one stored session. Label is its name (or id) and Detail a
// muted one-line summary (age, counts).
type SessionItem struct {
	ID     string
	Label  string
	Detail string
}

// SessionAction is what the sessions picker asks the caller to do.
type SessionAction int

const (
	SessionSelect SessionAction = iota + 1 // switch to SessionChoice.ID (closes the picker)
	SessionNew                             // start a new session (closes the picker)
	SessionRename                          // rename SessionChoice.ID to SessionChoice.Name (picker stays open)
	SessionDelete                          // delete SessionChoice.ID, already confirmed (picker stays open)
)

// SessionChoice carries a sessions-picker action.
type SessionChoice struct {
	Action SessionAction
	ID     string
	Name   string
}

// sessionsMode is the picker's input state.
type sessionsMode int

const (
	sessionsBrowse  sessionsMode = iota // moving the cursor
	sessionsRename                      // typing a new name for the cursor's session
	sessionsConfirm                     // waiting for y to delete the cursor's session
)

// sessionsOverlay lists the branch's sessions. Row 0 is the "new session"
// row; row i > 0 is spec.Items[i-1]. Keys: enter switches (or starts a new
// session on row 0), n new, r rename, d delete (confirmed with y), esc closes.
type sessionsOverlay struct {
	spec       SessionsSpec
	cursor     int
	offset     int
	mode       sessionsMode
	input      string // rename text
	notice     string // transient notice set by the overlay itself; cleared on the next key
	height     int
	popupWidth int
}

func (s *sessionsOverlay) open(spec SessionsSpec) {
	s.spec = spec
	s.mode = sessionsBrowse
	s.input, s.notice = "", ""
	s.offset = 0
	s.cursor = 0
	for i, it := range spec.Items {
		if it.ID == spec.ActiveID {
			s.cursor = i + 1
			break
		}
	}
}

// update swaps in a refreshed list (after a rename or delete), keeping the
// cursor on the same session when it still exists.
func (s *sessionsOverlay) update(spec SessionsSpec) {
	keep := ""
	if it, ok := s.currentItem(); ok {
		keep = it.ID
	}
	s.spec = spec
	s.mode = sessionsBrowse
	s.input = ""
	s.cursor = min(s.cursor, len(spec.Items))
	for i, it := range spec.Items {
		if it.ID == keep {
			s.cursor = i + 1
			break
		}
	}
}

func (s *sessionsOverlay) rows() int { return len(s.spec.Items) + 1 }

func (s *sessionsOverlay) currentItem() (SessionItem, bool) {
	if s.cursor < 1 || s.cursor > len(s.spec.Items) {
		return SessionItem{}, false
	}
	return s.spec.Items[s.cursor-1], true
}

func (s *sessionsOverlay) maxVisible() int {
	return max(min(s.rows(), s.height-sessionsChromeLines), 1)
}

func (s *sessionsOverlay) render(ctx RenderCtx, mgr *Manager) string {
	s.height = ctx.Height
	s.popupWidth = max(min(ctx.Width-sessionsMargin, sessionsMaxWidth), sessionsMinWidth)
	maxVisible := s.maxVisible()
	s.cursor = min(max(s.cursor, 0), s.rows()-1)
	s.offset = min(s.offset, max(s.rows()-maxVisible, 0))
	if s.cursor >= s.offset+maxVisible {
		s.offset = s.cursor - maxVisible + 1
	}
	if s.cursor < s.offset {
		s.offset = s.cursor
	}

	contentWidth := s.popupWidth - sessionsBorderPad
	muted := string(ctx.Resolver.Color(style.ColorKeyMutedFg))
	parts := []string{s.renderPrompt(ctx.Resolver, contentWidth), ""}
	end := min(s.rows(), s.offset+maxVisible)
	for i := s.offset; i < end; i++ {
		parts = append(parts, s.formatRow(i, contentWidth, ctx.Resolver))
	}
	if notice := s.currentNotice(); notice != "" {
		parts = append(parts, "", muted+"  "+ansi.Truncate(s.clean(notice), max(contentWidth-2, 0), "…")+string(style.ResetFg))
	}

	title := " sessions "
	if s.spec.Branch != "" {
		title = " sessions · " + runewidth.Truncate(s.clean(s.spec.Branch), max(s.popupWidth/2, 8), "…") + " "
	}
	box := ctx.Resolver.Style(style.StyleKeyFilePickerBox).Width(s.popupWidth).Render(strings.Join(parts, "\n"))
	return mgr.injectBorderTitle(box, title, borderEdgeText{
		popupWidth: s.popupWidth,
		accentFg:   string(ctx.Resolver.Color(style.ColorKeyAccentFg)),
		paneBg:     string(ctx.Resolver.Color(style.ColorKeyDiffPaneBg)),
	})
}

func (s *sessionsOverlay) currentNotice() string {
	if s.notice != "" {
		return s.notice
	}
	return s.spec.Notice
}

// renderPrompt is the line above the list: the key hints, the rename input,
// or the delete confirmation.
func (s *sessionsOverlay) renderPrompt(resolver Resolver, width int) string {
	muted := string(resolver.Color(style.ColorKeyMutedFg))
	switch s.mode {
	case sessionsRename:
		text := s.input
		if over := runewidth.StringWidth(text) - (width - 12); over > 0 {
			text = runewidth.TruncateLeft(text, over+1, "…")
		}
		return "  name: " + text + string(resolver.Color(style.ColorKeyAccentFg)) + "│" + string(style.ResetFg)
	case sessionsConfirm:
		it, _ := s.currentItem()
		msg := "delete " + s.clean(it.Label) + "? y to confirm, any other key cancels"
		return "  " + ansi.Truncate(msg, max(width-2, 0), "…")
	default:
		return "  " + muted + ansi.Truncate("enter switch · n new · r rename · d delete · esc close", max(width-2, 0), "…") + string(style.ResetFg)
	}
}

func (s *sessionsOverlay) formatRow(i, width int, resolver Resolver) string {
	selected := i == s.cursor
	var label, detail string
	if i == 0 {
		label = sessionsNewLabel
	} else {
		it := s.spec.Items[i-1]
		label, detail = s.clean(it.Label), s.clean(it.Detail)
		if it.ID == s.spec.ActiveID {
			label = "● " + label
		} else {
			label = "  " + label
		}
	}
	avail := max(width-2, 0)
	label = runewidth.Truncate(label, avail, "…")
	if rest := avail - runewidth.StringWidth(label) - 2; detail != "" && rest > 3 {
		detail = "  " + runewidth.Truncate(detail, rest, "…")
	} else {
		detail = ""
	}
	if selected {
		st := resolver.Style(style.StyleKeyFileSelected)
		styled := st.Render("> " + label + detail)
		if w := lipgloss.Width(styled); w < width {
			styled += st.Render(strings.Repeat(" ", width-w))
		}
		return styled
	}
	muted := string(resolver.Color(style.ColorKeyMutedFg))
	if detail == "" {
		return "  " + label
	}
	return "  " + label + muted + detail + string(style.ResetFg)
}

// clean flattens caller text to one safe line: escape sequences are removed
// whole, then remaining control and bidi bytes, then whitespace runs collapse.
func (s *sessionsOverlay) clean(text string) string {
	return strings.Join(strings.Fields(style.SanitizeFilenameForDisplay(ansi.Strip(text))), " ")
}

func (s *sessionsOverlay) handleKey(msg tea.KeyMsg, action keymap.Action) Outcome {
	s.notice = ""
	switch s.mode {
	case sessionsRename:
		return s.handleRenameKey(msg)
	case sessionsConfirm:
		s.mode = sessionsBrowse
		if msg.String() == "y" {
			if it, ok := s.currentItem(); ok {
				return Outcome{Kind: OutcomeSessionAction, SessionChoice: &SessionChoice{Action: SessionDelete, ID: it.ID}}
			}
		}
		return Outcome{Kind: OutcomeNone}
	default:
	}

	switch action {
	case keymap.ActionUp:
		s.moveCursorBy(-1)
		return Outcome{Kind: OutcomeNone}
	case keymap.ActionDown:
		s.moveCursorBy(1)
		return Outcome{Kind: OutcomeNone}
	case keymap.ActionSessions, keymap.ActionDismiss:
		return Outcome{Kind: OutcomeClosed}
	default:
	}

	switch msg.String() {
	case "enter":
		return s.chooseCurrent()
	case "esc", "q":
		return Outcome{Kind: OutcomeClosed}
	case "n":
		return Outcome{Kind: OutcomeSessionAction, SessionChoice: &SessionChoice{Action: SessionNew}}
	case "r":
		if it, ok := s.currentItem(); ok {
			s.mode = sessionsRename
			s.input = it.Label
			if it.Label == it.ID {
				s.input = ""
			}
		}
	case "d":
		it, ok := s.currentItem()
		switch {
		case !ok:
		case it.ID == s.spec.ActiveID:
			s.notice = "the session in use cannot be deleted — switch to another one first"
		default:
			s.mode = sessionsConfirm
		}
	}
	return Outcome{Kind: OutcomeNone}
}

func (s *sessionsOverlay) handleRenameKey(msg tea.KeyMsg) Outcome {
	if text, ok := printableKeyText(msg); ok {
		s.input += text
		return Outcome{Kind: OutcomeNone}
	}
	switch msg.Type {
	case tea.KeyBackspace:
		if s.input != "" {
			runes := []rune(s.input)
			s.input = string(runes[:len(runes)-1])
		}
	case tea.KeyEsc:
		s.mode = sessionsBrowse
		s.input = ""
	case tea.KeyEnter:
		s.mode = sessionsBrowse
		if it, ok := s.currentItem(); ok {
			name := strings.TrimSpace(s.input)
			s.input = ""
			return Outcome{Kind: OutcomeSessionAction, SessionChoice: &SessionChoice{Action: SessionRename, ID: it.ID, Name: name}}
		}
	default:
	}
	return Outcome{Kind: OutcomeNone}
}

func (s *sessionsOverlay) chooseCurrent() Outcome {
	if s.cursor == 0 {
		return Outcome{Kind: OutcomeSessionAction, SessionChoice: &SessionChoice{Action: SessionNew}}
	}
	it, ok := s.currentItem()
	if !ok {
		return Outcome{Kind: OutcomeNone}
	}
	return Outcome{Kind: OutcomeSessionAction, SessionChoice: &SessionChoice{Action: SessionSelect, ID: it.ID}}
}

func (s *sessionsOverlay) moveCursorBy(delta int) {
	s.cursor = min(max(s.cursor+delta, 0), s.rows()-1)
	if s.cursor < s.offset {
		s.offset = s.cursor
	}
	if mv := s.maxVisible(); s.cursor >= s.offset+mv {
		s.offset = s.cursor - mv + 1
	}
}

func (s *sessionsOverlay) handleMouse(msg tea.MouseMsg) Outcome {
	if msg.Action != tea.MouseActionPress || s.mode != sessionsBrowse {
		return Outcome{Kind: OutcomeNone}
	}
	switch msg.Button {
	case tea.MouseButtonWheelDown:
		s.moveCursorBy(1)
	case tea.MouseButtonWheelUp:
		s.moveCursorBy(-1)
	case tea.MouseButtonLeft:
		return s.handleLeftClick(msg.X, msg.Y)
	default:
	}
	return Outcome{Kind: OutcomeNone}
}

func (s *sessionsOverlay) handleLeftClick(localX, localY int) Outcome {
	// same box geometry as the file picker: y=0 border, y=1 top padding,
	// y=2 prompt line, y=3 blank separator, y=4+ rows; x=0 border, x=1 padding.
	const entriesTop = 4
	const horizChromeCols = 2
	if localX < horizChromeCols || localX >= s.popupWidth-horizChromeCols {
		return Outcome{Kind: OutcomeNone}
	}
	rel := localY - entriesTop
	if rel < 0 || rel >= s.maxVisible() {
		return Outcome{Kind: OutcomeNone}
	}
	idx := s.offset + rel
	if idx >= s.rows() {
		return Outcome{Kind: OutcomeNone}
	}
	s.cursor = idx
	return s.chooseCurrent()
}
