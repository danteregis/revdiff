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

func sessionsSpec() SessionsSpec {
	return SessionsSpec{
		Branch:   "feature/x",
		ActiveID: "s2",
		Items: []SessionItem{
			{ID: "s1", Label: "round three", Detail: "updated 1m ago · 4 reviewed"},
			{ID: "s2", Label: "s2", Detail: "updated 2h ago · 1 reviewed"},
			{ID: "s3", Label: "old\x1b[31m one", Detail: "updated 3d ago"},
		},
	}
}

func runeKey(r rune) tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}} }

func renderSessions(mgr *Manager) string {
	return ansi.Strip(mgr.Compose(strings.Repeat("\n", 29), RenderCtx{Width: 100, Height: 30, Resolver: style.PlainResolver()}))
}

func TestSessionsOverlay_OpenRender(t *testing.T) {
	mgr := NewManager()
	mgr.OpenSessions(sessionsSpec())
	assert.Equal(t, KindSessions, mgr.Kind())
	assert.Equal(t, 2, mgr.sessions.cursor, "the active session is preselected")

	out := renderSessions(mgr)
	assert.Contains(t, out, "sessions · feature/x")
	assert.Contains(t, out, "+ New session")
	assert.Contains(t, out, "round three")
	assert.Contains(t, out, "updated 1m ago · 4 reviewed")
	assert.Contains(t, out, "● s2", "the active session is marked")
	assert.Contains(t, out, "old one", "labels are sanitized")
	assert.Contains(t, out, "enter switch · n new · r rename · d delete · esc close")
}

func TestSessionsOverlay_Navigation(t *testing.T) {
	mgr := NewManager()
	mgr.OpenSessions(sessionsSpec())

	out := mgr.HandleKey(tea.KeyMsg{Type: tea.KeyDown}, keymap.ActionDown)
	assert.Equal(t, OutcomeNone, out.Kind)
	assert.Equal(t, 3, mgr.sessions.cursor)
	mgr.HandleKey(tea.KeyMsg{Type: tea.KeyDown}, keymap.ActionDown)
	assert.Equal(t, 3, mgr.sessions.cursor, "clamped at the end")

	out = mgr.HandleKey(tea.KeyMsg{Type: tea.KeyEnter}, "")
	require.Equal(t, OutcomeSessionAction, out.Kind)
	assert.Equal(t, &SessionChoice{Action: SessionSelect, ID: "s3"}, out.SessionChoice)
	assert.False(t, mgr.Active(), "selecting closes the picker")

	mgr.OpenSessions(sessionsSpec())
	for range 5 {
		mgr.HandleKey(tea.KeyMsg{Type: tea.KeyUp}, keymap.ActionUp)
	}
	assert.Equal(t, 0, mgr.sessions.cursor)
	out = mgr.HandleKey(tea.KeyMsg{Type: tea.KeyEnter}, "")
	assert.Equal(t, &SessionChoice{Action: SessionNew}, out.SessionChoice, "the first row starts a new session")
	assert.False(t, mgr.Active())

	mgr.OpenSessions(sessionsSpec())
	out = mgr.HandleKey(runeKey('n'), "")
	assert.Equal(t, &SessionChoice{Action: SessionNew}, out.SessionChoice)

	for _, closeKey := range []struct {
		msg    tea.KeyMsg
		action keymap.Action
	}{
		{tea.KeyMsg{Type: tea.KeyEsc}, keymap.ActionDismiss},
		{runeKey('S'), keymap.ActionSessions},
		{runeKey('q'), ""},
	} {
		mgr.OpenSessions(sessionsSpec())
		assert.Equal(t, OutcomeClosed, mgr.HandleKey(closeKey.msg, closeKey.action).Kind)
		assert.False(t, mgr.Active())
	}
}

func TestSessionsOverlay_Rename(t *testing.T) {
	mgr := NewManager()
	mgr.OpenSessions(sessionsSpec())
	mgr.HandleKey(runeKey('r'), "")
	assert.Equal(t, sessionsRename, mgr.sessions.mode)
	assert.Empty(t, mgr.sessions.input, "an unnamed session starts with an empty name")
	for _, r := range "deep dive" {
		mgr.HandleKey(runeKey(r), "")
	}
	mgr.HandleKey(tea.KeyMsg{Type: tea.KeyBackspace}, "")
	assert.Contains(t, renderSessions(mgr), "name: deep div")
	out := mgr.HandleKey(tea.KeyMsg{Type: tea.KeyEnter}, "")
	require.Equal(t, OutcomeSessionAction, out.Kind)
	assert.Equal(t, &SessionChoice{Action: SessionRename, ID: "s2", Name: "deep div"}, out.SessionChoice)
	assert.True(t, mgr.Active(), "rename keeps the picker open")
	assert.Equal(t, sessionsBrowse, mgr.sessions.mode)

	mgr.HandleKey(tea.KeyMsg{Type: tea.KeyUp}, keymap.ActionUp)
	mgr.HandleKey(runeKey('r'), "")
	assert.Equal(t, "round three", mgr.sessions.input, "a named session starts from its name")
	mgr.HandleKey(tea.KeyMsg{Type: tea.KeyEsc}, "")
	assert.Equal(t, sessionsBrowse, mgr.sessions.mode)
	assert.True(t, mgr.Active(), "esc cancels the rename, not the picker")

	mgr.HandleKey(tea.KeyMsg{Type: tea.KeyUp}, keymap.ActionUp)
	mgr.HandleKey(runeKey('r'), "")
	assert.Equal(t, sessionsBrowse, mgr.sessions.mode, "the new-session row cannot be renamed")
}

func TestSessionsOverlay_Delete(t *testing.T) {
	mgr := NewManager()
	mgr.OpenSessions(sessionsSpec())
	mgr.HandleKey(runeKey('d'), "")
	assert.Equal(t, sessionsBrowse, mgr.sessions.mode)
	assert.Contains(t, renderSessions(mgr), "the session in use cannot be deleted")

	mgr.HandleKey(tea.KeyMsg{Type: tea.KeyDown}, keymap.ActionDown)
	assert.NotContains(t, renderSessions(mgr), "cannot be deleted", "the notice clears on the next key")
	mgr.HandleKey(runeKey('d'), "")
	assert.Equal(t, sessionsConfirm, mgr.sessions.mode)
	assert.Contains(t, renderSessions(mgr), "delete old one? y to confirm")
	out := mgr.HandleKey(runeKey('n'), "")
	assert.Equal(t, OutcomeNone, out.Kind, "any other key cancels")
	assert.Equal(t, sessionsBrowse, mgr.sessions.mode)

	mgr.HandleKey(runeKey('d'), "")
	out = mgr.HandleKey(runeKey('y'), "")
	require.Equal(t, OutcomeSessionAction, out.Kind)
	assert.Equal(t, &SessionChoice{Action: SessionDelete, ID: "s3"}, out.SessionChoice)
	assert.True(t, mgr.Active())

	spec := sessionsSpec()
	spec.Items = spec.Items[:2]
	spec.Notice = "deleted"
	mgr.UpdateSessions(spec)
	assert.Equal(t, 2, mgr.sessions.cursor, "the cursor stays in range after the row is gone")
	assert.Contains(t, renderSessions(mgr), "deleted")
}

func TestSessionsOverlay_UpdateKeepsCursor(t *testing.T) {
	mgr := NewManager()
	mgr.OpenSessions(sessionsSpec())
	spec := sessionsSpec()
	spec.Items = append([]SessionItem{{ID: "s0", Label: "newest"}}, spec.Items...)
	mgr.UpdateSessions(spec)
	assert.Equal(t, 3, mgr.sessions.cursor, "still on s2")

	other := NewManager()
	other.UpdateSessions(spec) // no-op when the picker is not open
	assert.False(t, other.Active())
}

func TestSessionsOverlay_Mouse(t *testing.T) {
	mgr := NewManager()
	mgr.OpenSessions(sessionsSpec())
	renderSessions(mgr) // records the popup bounds

	mgr.HandleMouse(tea.MouseMsg{Button: tea.MouseButtonWheelDown, Action: tea.MouseActionPress})
	assert.Equal(t, 3, mgr.sessions.cursor)
	mgr.HandleMouse(tea.MouseMsg{Button: tea.MouseButtonWheelUp, Action: tea.MouseActionPress})
	assert.Equal(t, 2, mgr.sessions.cursor)

	b := mgr.bounds
	out := mgr.HandleMouse(tea.MouseMsg{X: b.x + 5, Y: b.y + 4 + 1, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress})
	require.Equal(t, OutcomeSessionAction, out.Kind)
	assert.Equal(t, &SessionChoice{Action: SessionSelect, ID: "s1"}, out.SessionChoice, "row 1 is the first session")
	assert.False(t, mgr.Active())

	mgr.OpenSessions(sessionsSpec())
	renderSessions(mgr)
	b = mgr.bounds
	assert.Equal(t, OutcomeNone, mgr.HandleMouse(tea.MouseMsg{X: b.x + 5, Y: b.y + 2, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress}).Kind,
		"the prompt line is not a row")
	assert.Equal(t, OutcomeNone, mgr.HandleMouse(tea.MouseMsg{X: b.x, Y: b.y + 4, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress}).Kind,
		"the border is not a row")
	out = mgr.HandleMouse(tea.MouseMsg{X: b.x + 5, Y: b.y + 4, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress})
	assert.Equal(t, &SessionChoice{Action: SessionNew}, out.SessionChoice)
}
