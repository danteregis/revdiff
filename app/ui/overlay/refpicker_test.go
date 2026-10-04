package overlay

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/umputun/revdiff/app/keymap"
	"github.com/umputun/revdiff/app/ui/style"
)

func refPickerSpec() RefPickerSpec {
	return RefPickerSpec{
		Current:  "working tree",
		ActiveID: "branch:feature",
		Items: []RefItem{
			{ID: "original", Section: "original", Label: "working tree", Detail: "as started"},
			{ID: "pr:12", Section: "pull requests", Label: "#12 Add widget", Detail: "widget → main · alice"},
			{ID: "branch:main", Section: "branches", Label: "main", Detail: "base"},
			{ID: "branch:feature", Section: "branches", Label: "feature", Detail: "origin/main...feature"},
		},
	}
}

func refRenderCtx(height int) RenderCtx {
	return RenderCtx{Width: 100, Height: height, Resolver: style.PlainResolver()}
}

func typeRunes(mgr *Manager, s string) {
	for _, r := range s {
		mgr.HandleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}}, "")
	}
}

func TestRefPicker_OpenGroupsSectionsAndPlacesCursor(t *testing.T) {
	spec := refPickerSpec()
	mgr := NewManager()
	mgr.OpenRefPicker(spec)
	require.Equal(t, KindRefPicker, mgr.Kind())

	headers := []string{}
	for _, row := range mgr.refPick.rows {
		if !row.selectable() {
			headers = append(headers, row.header)
		}
	}
	assert.Equal(t, []string{"original", "pull requests", "branches"}, headers)
	row, ok := mgr.refPick.currentRow()
	require.True(t, ok)
	assert.Equal(t, "branch:feature", mgr.refPick.spec.Items[row.item].ID)

	spec.Items[0].Label = "mutated"
	assert.Equal(t, "working tree", mgr.refPick.spec.Items[0].Label, "spec is copied")

	out := ansi.Strip(mgr.refPick.render(refRenderCtx(40), mgr))
	assert.Contains(t, out, "review: working tree")
	assert.Contains(t, out, "pull requests")
	assert.Contains(t, out, "#12 Add widget")
	assert.Contains(t, out, "> feature  origin/main...feature")
}

func TestRefPicker_NavigationSkipsHeaders(t *testing.T) {
	mgr := NewManager()
	spec := refPickerSpec()
	spec.ActiveID = ""
	mgr.OpenRefPicker(spec)
	ids := func() string {
		row, ok := mgr.refPick.currentRow()
		require.True(t, ok)
		return mgr.refPick.spec.Items[row.item].ID
	}
	assert.Equal(t, "original", ids())
	mgr.HandleKey(tea.KeyMsg{Type: tea.KeyDown}, keymap.ActionDown)
	assert.Equal(t, "pr:12", ids())
	mgr.HandleKey(tea.KeyMsg{Type: tea.KeyDown}, keymap.ActionDown)
	assert.Equal(t, "branch:main", ids())
	mgr.HandleKey(tea.KeyMsg{Type: tea.KeyDown}, keymap.ActionDown)
	mgr.HandleKey(tea.KeyMsg{Type: tea.KeyDown}, keymap.ActionDown)
	assert.Equal(t, "branch:feature", ids(), "stops at the last row")
	mgr.HandleKey(tea.KeyMsg{Type: tea.KeyUp}, keymap.ActionUp)
	mgr.HandleKey(tea.KeyMsg{Type: tea.KeyUp}, keymap.ActionUp)
	mgr.HandleKey(tea.KeyMsg{Type: tea.KeyUp}, keymap.ActionUp)
	mgr.HandleKey(tea.KeyMsg{Type: tea.KeyUp}, keymap.ActionUp)
	assert.Equal(t, "original", ids(), "stops at the first row")

	out := mgr.HandleKey(tea.KeyMsg{Type: tea.KeyEnter}, keymap.ActionConfirm)
	assert.Equal(t, OutcomeRefChosen, out.Kind)
	require.NotNil(t, out.RefChoice)
	assert.Equal(t, RefChoice{ID: "original"}, *out.RefChoice)
	assert.False(t, mgr.Active(), "choosing closes the overlay")
}

func TestRefPicker_FilterMatchesLabelAndDetailAndOffersRawRef(t *testing.T) {
	mgr := NewManager()
	mgr.OpenRefPicker(refPickerSpec())

	typeRunes(mgr, "ALICE")
	require.Len(t, mgr.refPick.rows, 4, "pr header + pr + custom header + raw row")
	row, ok := mgr.refPick.currentRow()
	require.True(t, ok)
	assert.Equal(t, "pr:12", mgr.refPick.spec.Items[row.item].ID)
	assert.True(t, mgr.refPick.rows[3].raw)

	mgr.HandleKey(tea.KeyMsg{Type: tea.KeyEsc}, keymap.ActionDismiss)
	assert.True(t, mgr.Active(), "esc with a filter clears it")
	assert.Empty(t, mgr.refPick.filter)

	typeRunes(mgr, "main~3..main")
	rows := mgr.refPick.rows
	require.Len(t, rows, 2, "nothing matches: only the custom ref row")
	assert.True(t, rows[1].raw)
	out := ansi.Strip(mgr.refPick.render(refRenderCtx(40), mgr))
	assert.Contains(t, out, `use "main~3..main"`)

	res := mgr.HandleKey(tea.KeyMsg{Type: tea.KeyEnter}, keymap.ActionConfirm)
	assert.Equal(t, OutcomeRefChosen, res.Kind)
	assert.Equal(t, RefChoice{Raw: "main~3..main"}, *res.RefChoice)
}

func TestRefPicker_PrintableKeysFilterBeforeActions(t *testing.T) {
	mgr := NewManager()
	mgr.OpenRefPicker(refPickerSpec())
	// "b" is the default switch_ref binding and j/k are up/down: all must type.
	mgr.HandleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'b'}}, keymap.ActionSwitchRef)
	mgr.HandleKey(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'j'}}, keymap.ActionDown)
	assert.Equal(t, "bj", mgr.refPick.filter)
	assert.True(t, mgr.Active())

	mgr.HandleKey(tea.KeyMsg{Type: tea.KeyBackspace}, "")
	assert.Equal(t, "b", mgr.refPick.filter)

	out := mgr.HandleKey(tea.KeyMsg{Type: tea.KeyF1}, keymap.ActionSwitchRef)
	assert.Equal(t, OutcomeClosed, out.Kind, "a non-printable switch_ref binding toggles the picker closed")
	assert.False(t, mgr.Active())
}

func TestRefPicker_EscWithoutFilterCloses(t *testing.T) {
	mgr := NewManager()
	mgr.OpenRefPicker(refPickerSpec())
	out := mgr.HandleKey(tea.KeyMsg{Type: tea.KeyEsc}, keymap.ActionDismiss)
	assert.Equal(t, OutcomeClosed, out.Kind)
	assert.False(t, mgr.Active())
}

func TestRefPicker_UpdateKeepsFilterAndCursor(t *testing.T) {
	mgr := NewManager()
	spec := RefPickerSpec{Items: []RefItem{{ID: "original", Section: "original", Label: "HEAD~3"}}, Loading: true}
	mgr.OpenRefPicker(spec)
	out := ansi.Strip(mgr.refPick.render(refRenderCtx(40), mgr))
	assert.Contains(t, out, "loading…")
	assert.Contains(t, out, "switch review", "no current label falls back to the generic title")

	typeRunes(mgr, "fea")
	full := refPickerSpec()
	full.Notices = []string{"pull requests unavailable: gh CLI not found on PATH"}
	mgr.UpdateRefPicker(full)
	assert.Equal(t, "fea", mgr.refPick.filter)
	row, ok := mgr.refPick.currentRow()
	require.True(t, ok)
	assert.Equal(t, "branch:feature", mgr.refPick.spec.Items[row.item].ID)
	out = ansi.Strip(mgr.refPick.render(refRenderCtx(40), mgr))
	assert.NotContains(t, out, "loading…")
	assert.Contains(t, out, "pull requests unavailable")

	// the raw row keeps the cursor across an update
	mgr.HandleKey(tea.KeyMsg{Type: tea.KeyDown}, keymap.ActionDown)
	row, ok = mgr.refPick.currentRow()
	require.True(t, ok)
	require.True(t, row.raw)
	mgr.UpdateRefPicker(full)
	row, ok = mgr.refPick.currentRow()
	require.True(t, ok)
	assert.True(t, row.raw)

	mgr.Close()
	mgr.UpdateRefPicker(full) // no-op when inactive
	assert.False(t, mgr.Active())
}

func TestRefPicker_SanitizesExternalText(t *testing.T) {
	mgr := NewManager()
	mgr.OpenRefPicker(RefPickerSpec{Items: []RefItem{
		{ID: "pr:1", Section: "pull requests", Label: "#1 evil\x1b[31mred\x1b[0m\ntitle", Detail: "x\x07y"},
	}})
	out := mgr.refPick.render(refRenderCtx(40), mgr)
	assert.NotContains(t, out, "\x1b[31m")
	assert.NotContains(t, out, "\x07")
	assert.Contains(t, ansi.Strip(out), "#1 evil", "control bytes cannot break the row")
	assert.Contains(t, ansi.Strip(out), "title  xy")
}

func TestRefPicker_MouseClickAndWheel(t *testing.T) {
	mgr := NewManager()
	spec := refPickerSpec()
	spec.ActiveID = ""
	mgr.OpenRefPicker(spec)
	base := strings.Repeat(strings.Repeat(" ", 100)+"\n", 39) + strings.Repeat(" ", 100)
	mgr.Compose(base, refRenderCtx(40))
	b := mgr.bounds
	require.Positive(t, b.w)

	// rows: 0 header(original) 1 original 2 header(pr) 3 pr 4 header(branches) 5 main 6 feature
	click := func(row int) Outcome {
		return mgr.HandleMouse(tea.MouseMsg{X: b.x + 5, Y: b.y + 4 + row, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress})
	}
	assert.Equal(t, OutcomeNone, click(2).Kind, "headers are not clickable")
	assert.True(t, mgr.Active())

	mgr.HandleMouse(tea.MouseMsg{Button: tea.MouseButtonWheelDown, Action: tea.MouseActionPress})
	row, ok := mgr.refPick.currentRow()
	require.True(t, ok)
	assert.Equal(t, "pr:12", mgr.refPick.spec.Items[row.item].ID)

	out := click(6)
	assert.Equal(t, OutcomeRefChosen, out.Kind)
	assert.Equal(t, "branch:feature", out.RefChoice.ID)
	assert.False(t, mgr.Active())
}

func TestRefPicker_ScrollsLongListsKeepingCursorVisible(t *testing.T) {
	items := make([]RefItem, 0, 41)
	items = append(items, RefItem{ID: "original", Section: "original", Label: "orig"})
	for i := range 40 {
		items = append(items, RefItem{ID: "branch:b" + string(rune('a'+i%26)) + strings.Repeat("x", i/26), Section: "branches", Label: "b" + string(rune('a'+i))})
	}
	mgr := NewManager()
	mgr.OpenRefPicker(RefPickerSpec{Items: items})
	mgr.refPick.render(refRenderCtx(20), mgr)
	for range 30 {
		mgr.HandleKey(tea.KeyMsg{Type: tea.KeyDown}, keymap.ActionDown)
	}
	visible := mgr.refPick.maxVisible()
	assert.GreaterOrEqual(t, mgr.refPick.cursor, mgr.refPick.offset)
	assert.Less(t, mgr.refPick.cursor, mgr.refPick.offset+visible)
	out := ansi.Strip(mgr.refPick.render(refRenderCtx(20), mgr))
	assert.Contains(t, out, "> "+items[30].Label)
}
