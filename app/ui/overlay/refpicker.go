package overlay

import (
	"slices"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/mattn/go-runewidth"

	"github.com/umputun/revdiff/app/keymap"
	"github.com/umputun/revdiff/app/ui/style"
)

const (
	refPickerMaxWidth    = 100
	refPickerMinWidth    = 30
	refPickerMargin      = 10
	refPickerBorderPad   = 4
	refPickerChromeLines = 10
	refPickerRawSection  = "custom ref"
)

// refPickerRow is one rendered line of the entry list: either a section
// header (item == -1), a spec item (item >= 0), or the typed-text row that
// uses the filter as a raw ref (raw == true).
type refPickerRow struct {
	header string
	item   int
	raw    bool
}

func (r refPickerRow) selectable() bool { return r.header == "" }

// refPickerOverlay is the filterable review-target switcher: sections of
// items (original ref, pull requests, branches) supplied by the caller, plus a
// trailing "use typed text as a ref" row whenever the filter is non-empty.
type refPickerOverlay struct {
	spec       RefPickerSpec
	filter     string
	rows       []refPickerRow
	cursor     int // index into rows, always on a selectable row when any exists
	offset     int // first visible row
	height     int
	popupWidth int
}

func (r *refPickerOverlay) open(spec RefPickerSpec) {
	r.spec = r.cloneSpec(spec)
	r.filter = ""
	r.offset = 0
	r.rebuildRows()
	r.cursor = r.firstSelectable()
	for i, row := range r.rows {
		if row.item >= 0 && row.selectable() && !row.raw && r.spec.Items[row.item].ID == spec.ActiveID {
			r.cursor = i
			break
		}
	}
}

// update swaps in a refreshed spec (async lists landed) while keeping the
// typed filter and, when it is still present, the item under the cursor.
func (r *refPickerOverlay) update(spec RefPickerSpec) {
	keepID, keepRaw := "", false
	if row, ok := r.currentRow(); ok {
		// the raw row is kept only when the user moved onto it deliberately; when it
		// was selected because nothing else matched, newly arrived matches win.
		keepRaw = row.raw && r.cursor != r.firstSelectable()
		if !row.raw {
			keepID = r.spec.Items[row.item].ID
		}
	}
	r.spec = r.cloneSpec(spec)
	r.rebuildRows()
	r.cursor = r.firstSelectable()
	for i, row := range r.rows {
		if (keepRaw && row.raw) || (!keepRaw && keepID != "" && !row.raw && row.selectable() && r.spec.Items[row.item].ID == keepID) {
			r.cursor = i
			break
		}
	}
}

func (r *refPickerOverlay) cloneSpec(spec RefPickerSpec) RefPickerSpec {
	spec.Items = slices.Clone(spec.Items)
	spec.Notices = slices.Clone(spec.Notices)
	return spec
}

// rebuildRows recomputes the visible rows from the spec and filter: matching
// items grouped under a header whenever the section changes, then the typed
// ref row when a filter is set.
func (r *refPickerOverlay) rebuildRows() {
	needle := strings.ToLower(r.filter)
	r.rows = r.rows[:0]
	section := ""
	for i, it := range r.spec.Items {
		if needle != "" && !strings.Contains(strings.ToLower(it.Label+" "+it.Detail), needle) {
			continue
		}
		if it.Section != section || len(r.rows) == 0 {
			section = it.Section
			r.rows = append(r.rows, refPickerRow{header: section, item: -1})
		}
		r.rows = append(r.rows, refPickerRow{item: i})
	}
	if strings.TrimSpace(r.filter) != "" {
		r.rows = append(r.rows, refPickerRow{header: refPickerRawSection, item: -1}, refPickerRow{item: -1, raw: true})
	}
	r.offset = 0
}

func (r *refPickerOverlay) firstSelectable() int {
	for i, row := range r.rows {
		if row.selectable() {
			return i
		}
	}
	return 0
}

func (r *refPickerOverlay) currentRow() (refPickerRow, bool) {
	if r.cursor < 0 || r.cursor >= len(r.rows) || !r.rows[r.cursor].selectable() {
		return refPickerRow{}, false
	}
	return r.rows[r.cursor], true
}

func (r *refPickerOverlay) applyFilter() {
	r.rebuildRows()
	r.cursor = r.firstSelectable()
}

// footerLines are the muted status lines rendered below the entries: the
// loading indicator and caller-supplied notices (e.g. "pull requests
// unavailable: …"). They sit below the list so click mapping of entry rows is
// unaffected.
func (r *refPickerOverlay) footerLines() []string {
	var lines []string
	if r.spec.Loading {
		lines = append(lines, "loading…")
	}
	lines = append(lines, r.spec.Notices...)
	return lines
}

func (r *refPickerOverlay) maxVisible() int {
	avail := r.height - refPickerChromeLines - len(r.footerLines())
	return max(min(len(r.rows), avail), 1)
}

func (r *refPickerOverlay) render(ctx RenderCtx, mgr *Manager) string {
	r.height = ctx.Height
	r.popupWidth = max(min(ctx.Width-refPickerMargin, refPickerMaxWidth), refPickerMinWidth)
	maxVisible := r.maxVisible()
	r.clampScroll(maxVisible)

	contentWidth := r.popupWidth - refPickerBorderPad
	muted := string(ctx.Resolver.Color(style.ColorKeyMutedFg))
	parts := []string{r.renderFilter(ctx.Resolver), ""}
	if len(r.rows) == 0 {
		parts = append(parts, muted+"  no matches"+string(style.ResetFg))
	} else {
		end := min(len(r.rows), r.offset+maxVisible)
		for i := r.offset; i < end; i++ {
			parts = append(parts, r.formatRow(r.rows[i], contentWidth, i == r.cursor, ctx.Resolver))
		}
	}
	for _, line := range r.footerLines() {
		clean := runewidth.Truncate(style.SanitizeFilenameForDisplay(line), max(contentWidth-2, 0), "…")
		parts = append(parts, muted+"  "+clean+string(style.ResetFg))
	}

	title := " switch review "
	if r.spec.Current != "" {
		title = " review: " + runewidth.Truncate(style.SanitizeFilenameForDisplay(r.spec.Current), max(r.popupWidth-16, 4), "…") + " "
	}
	box := ctx.Resolver.Style(style.StyleKeyFilePickerBox).Width(r.popupWidth).Render(strings.Join(parts, "\n"))
	return mgr.injectBorderTitle(box, title, borderEdgeText{
		popupWidth: r.popupWidth,
		accentFg:   string(ctx.Resolver.Color(style.ColorKeyAccentFg)),
		paneBg:     string(ctx.Resolver.Color(style.ColorKeyDiffPaneBg)),
	})
}

func (r *refPickerOverlay) clampScroll(maxVisible int) {
	if len(r.rows) == 0 {
		r.cursor, r.offset = 0, 0
		return
	}
	r.cursor = min(max(r.cursor, 0), len(r.rows)-1)
	r.offset = min(r.offset, max(len(r.rows)-maxVisible, 0))
	if r.cursor >= r.offset+maxVisible {
		r.offset = r.cursor - maxVisible + 1
	}
	if r.cursor < r.offset {
		r.offset = r.cursor
	}
	// keep the header of the cursor's section in view when it is just above
	if r.offset > 0 && r.offset == r.cursor && !r.rows[r.offset-1].selectable() {
		r.offset--
	}
}

func (r *refPickerOverlay) renderFilter(resolver Resolver) string {
	if r.filter == "" {
		return "  " + string(resolver.Color(style.ColorKeyMutedFg)) + "type to filter, or a ref like main...feature" + string(style.ResetFg)
	}
	filterWidth := max(r.popupWidth-refPickerBorderPad-3, 0)
	display := runewidth.Truncate(style.SanitizeFilenameForDisplay(r.filter), filterWidth, "…")
	return "  " + display + string(resolver.Color(style.ColorKeyAccentFg)) + "│" + string(style.ResetFg)
}

func (r *refPickerOverlay) formatRow(row refPickerRow, width int, selected bool, resolver Resolver) string {
	if !row.selectable() {
		accent := string(resolver.Color(style.ColorKeyAccentFg))
		return " " + accent + runewidth.Truncate(style.SanitizeFilenameForDisplay(row.header), max(width-1, 0), "…") + string(style.ResetFg)
	}
	var label, detail string
	if row.raw {
		label = "use \"" + strings.TrimSpace(r.filter) + "\""
	} else {
		it := r.spec.Items[row.item]
		label, detail = it.Label, it.Detail
	}
	label = style.SanitizeFilenameForDisplay(label)
	detail = style.SanitizeFilenameForDisplay(detail)
	avail := max(width-4, 0) // "> " / "  " prefix plus indent under the header
	label = runewidth.Truncate(label, avail, "…")
	detailRoom := avail - runewidth.StringWidth(label) - 2
	if detail != "" && detailRoom >= 4 {
		detail = runewidth.Truncate(detail, detailRoom, "…")
	} else {
		detail = ""
	}

	if selected {
		text := "> " + label
		if detail != "" {
			text += "  " + detail
		}
		entryStyle := resolver.Style(style.StyleKeyFileSelected)
		styled := entryStyle.Render("  " + text)
		if w := lipgloss.Width(styled); w < width {
			styled += entryStyle.Render(strings.Repeat(" ", width-w))
		}
		return styled
	}
	normal := string(resolver.Color(style.ColorKeyNormalFg))
	muted := string(resolver.Color(style.ColorKeyMutedFg))
	out := "    " + normal + label
	if normal != "" {
		out += string(style.ResetFg)
	}
	if detail != "" {
		out += "  " + muted + detail
		if muted != "" {
			out += string(style.ResetFg)
		}
	}
	return out
}

func (r *refPickerOverlay) handleKey(msg tea.KeyMsg, action keymap.Action) Outcome {
	if r.appendPrintableRunes(msg) {
		return Outcome{Kind: OutcomeNone}
	}
	switch action {
	case keymap.ActionSwitchRef:
		return Outcome{Kind: OutcomeClosed}
	case keymap.ActionUp:
		r.moveCursorBy(-1)
		return Outcome{Kind: OutcomeNone}
	case keymap.ActionDown:
		r.moveCursorBy(1)
		return Outcome{Kind: OutcomeNone}
	default:
	}

	switch msg.Type {
	case tea.KeyEnter:
		return r.chooseCurrent()
	case tea.KeyEsc:
		if r.filter == "" {
			return Outcome{Kind: OutcomeClosed}
		}
		r.filter = ""
		r.applyFilter()
		return Outcome{Kind: OutcomeNone}
	case tea.KeyBackspace:
		if r.filter != "" {
			runes := []rune(r.filter)
			r.filter = string(runes[:len(runes)-1])
			r.applyFilter()
		}
		return Outcome{Kind: OutcomeNone}
	default:
		return Outcome{Kind: OutcomeNone}
	}
}

// appendPrintableRunes mirrors the file picker: unmodified printable input
// (and space) always extends the filter, ahead of any action it is bound to.
func (r *refPickerOverlay) appendPrintableRunes(msg tea.KeyMsg) bool {
	text, ok := printableKeyText(msg)
	if !ok {
		return false
	}
	r.filter += text
	r.applyFilter()
	return true
}

func (r *refPickerOverlay) chooseCurrent() Outcome {
	row, ok := r.currentRow()
	if !ok {
		return Outcome{Kind: OutcomeNone}
	}
	if row.raw {
		return Outcome{Kind: OutcomeRefChosen, RefChoice: &RefChoice{Raw: strings.TrimSpace(r.filter)}}
	}
	return Outcome{Kind: OutcomeRefChosen, RefChoice: &RefChoice{ID: r.spec.Items[row.item].ID}}
}

// moveCursorBy steps the cursor by delta selectable rows, skipping headers.
func (r *refPickerOverlay) moveCursorBy(delta int) {
	step := 1
	if delta < 0 {
		step, delta = -1, -delta
	}
	for range delta {
		next := r.cursor + step
		for next >= 0 && next < len(r.rows) && !r.rows[next].selectable() {
			next += step
		}
		if next < 0 || next >= len(r.rows) {
			break
		}
		r.cursor = next
	}
	maxVisible := r.maxVisible()
	if r.cursor < r.offset {
		r.offset = r.cursor
	}
	if r.cursor >= r.offset+maxVisible {
		r.offset = r.cursor - maxVisible + 1
	}
}

func (r *refPickerOverlay) handleMouse(msg tea.MouseMsg) Outcome {
	if msg.Action != tea.MouseActionPress {
		return Outcome{Kind: OutcomeNone}
	}
	switch msg.Button {
	case tea.MouseButtonWheelDown:
		r.moveCursorBy(r.wheelStep(msg.Shift))
	case tea.MouseButtonWheelUp:
		r.moveCursorBy(-r.wheelStep(msg.Shift))
	case tea.MouseButtonLeft:
		return r.handleLeftClick(msg.X, msg.Y)
	default:
		// other mouse buttons and horizontal wheels are intentionally ignored
	}
	return Outcome{Kind: OutcomeNone}
}

func (r *refPickerOverlay) wheelStep(shift bool) int {
	if !shift {
		return 1
	}
	return max(r.maxVisible()/2, 1)
}

func (r *refPickerOverlay) handleLeftClick(localX, localY int) Outcome {
	// same box geometry as the file picker (shared StyleKeyFilePickerBox):
	// y=0 border, y=1 top padding, y=2 filter, y=3 blank, y=4+ rows.
	// header rows are not selectable, so clicking one is a no-op.
	const entriesTop = 4
	const horizChromeCols = 2
	if localX < horizChromeCols || localX >= r.popupWidth-horizChromeCols {
		return Outcome{Kind: OutcomeNone}
	}
	rel := localY - entriesTop
	if rel < 0 || rel >= r.maxVisible() {
		return Outcome{Kind: OutcomeNone}
	}
	idx := r.offset + rel
	if idx < 0 || idx >= len(r.rows) || !r.rows[idx].selectable() {
		return Outcome{Kind: OutcomeNone}
	}
	r.cursor = idx
	return r.chooseCurrent()
}
