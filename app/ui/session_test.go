package ui

import (
	"errors"
	"maps"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/umputun/revdiff/app/annotation"
	"github.com/umputun/revdiff/app/diff"
	"github.com/umputun/revdiff/app/session"
	"github.com/umputun/revdiff/app/ui/mocks"
)

// fakeSessionStore keeps the latest session per branch in memory and records
// every Open request and every saved snapshot.
type fakeSessionStore struct {
	byBranch map[string]*session.Session
	opens    []session.Request
	saves    []session.Session
	openErr  error
	saveErr  error
	mismatch bool
}

func newFakeSessionStore() *fakeSessionStore {
	return &fakeSessionStore{byBranch: map[string]*session.Session{}}
}

func (f *fakeSessionStore) Open(req session.Request) (session.Opened, error) {
	f.opens = append(f.opens, req)
	if f.openErr != nil {
		return session.Opened{}, f.openErr
	}
	branch := req.Branch
	if branch == "" {
		branch = "feature"
	}
	if s, ok := f.byBranch[branch]; ok && !req.Fresh {
		cp := cloneSession(s)
		return session.Opened{Session: cp, Resumed: true, FingerprintMismatch: f.mismatch}, nil
	}
	return session.Opened{Session: &session.Session{ID: "new", Branch: branch, Name: req.Name, Reviewed: map[string]string{}}}, nil
}

func (f *fakeSessionStore) Save(s *session.Session) error {
	f.saves = append(f.saves, *cloneSession(s))
	return f.saveErr
}

func (f *fakeSessionStore) lastSave(t *testing.T) session.Session {
	t.Helper()
	require.NotEmpty(t, f.saves, "expected a session save")
	return f.saves[len(f.saves)-1]
}

func cloneSession(s *session.Session) *session.Session {
	cp := *s
	cp.Reviewed = maps.Clone(s.Reviewed)
	if cp.Reviewed == nil {
		cp.Reviewed = map[string]string{}
	}
	return &cp
}

var sessionTestDiffs = map[string][]diff.DiffLine{
	"a.go": {{NewNum: 1, Content: "package a", ChangeType: diff.ChangeAdd}},
	"b.go": {{NewNum: 1, Content: "package b", ChangeType: diff.ChangeAdd}},
	"c.go": {{NewNum: 1, Content: "package c", ChangeType: diff.ChangeAdd}},
}

func sessionTestFingerprint(path string) string {
	return diff.FileFingerprint(diff.FileEntry{Path: path}, sessionTestDiffs[path])
}

// sessionModel builds a model over files whose diffs come from
// sessionTestDiffs, wired to store.
func sessionModel(t *testing.T, store *fakeSessionStore, files []string, cfg ModelConfig) Model {
	t.Helper()
	entries := make([]diff.FileEntry, 0, len(files))
	for _, f := range files {
		entries = append(entries, diff.FileEntry{Path: f})
	}
	renderer := &mocks.RendererMock{
		ChangedFilesFunc: func(string, bool) ([]diff.FileEntry, error) { return entries, nil },
		FileDiffFunc: func(req diff.FileDiffRequest) ([]diff.DiffLine, error) {
			return sessionTestDiffs[req.Path], nil
		},
	}
	if store != nil {
		cfg.Sessions = store
	}
	m := testNewModel(t, renderer, annotation.NewStore(), noopHighlighter(), cfg)
	m.layout.width, m.layout.height = 120, 40
	m.ready = true
	return m
}

// loadAll delivers the file list and the first file's diff.
func loadAll(t *testing.T, m Model) Model {
	t.Helper()
	m, cmd := feed(t, m, m.loadFiles()())
	return settle(t, m, cmd)
}

// settle runs cmd and feeds every resulting message (batches flattened) back
// into the model, a few rounds deep.
func settle(t *testing.T, m Model, cmd tea.Cmd) Model {
	t.Helper()
	queue := []tea.Cmd{cmd}
	for round := 0; round < 8 && len(queue) > 0; round++ {
		var next []tea.Cmd
		for _, c := range queue {
			if c == nil {
				continue
			}
			msg := c()
			if batch, ok := msg.(tea.BatchMsg); ok {
				next = append(next, batch...)
				continue
			}
			var out tea.Cmd
			m, out = feed(t, m, msg)
			next = append(next, out)
		}
		queue = next
	}
	return m
}

func TestSession_ResumeValidatesMarks(t *testing.T) {
	store := newFakeSessionStore()
	store.byBranch["feature"] = &session.Session{ID: "s1", Branch: "feature", Reviewed: map[string]string{
		"a.go":    sessionTestFingerprint("a.go"),
		"b.go":    "fingerprint-before-the-model-changed-it",
		"gone.go": "x",
	}}
	m := sessionModel(t, store, []string{"a.go", "b.go", "c.go"}, ModelConfig{Ref: "main", SessionName: "", NewSession: false})
	require.Len(t, store.opens, 1)
	assert.Equal(t, session.Request{Ref: "main"}, store.opens[0])

	m = loadAll(t, m)
	assert.True(t, m.tree.IsReviewed("a.go"), "unchanged file stays reviewed")
	assert.False(t, m.tree.IsReviewed("b.go"))
	assert.True(t, m.tree.IsChangedSinceReview("b.go"), "changed file is flagged")
	assert.False(t, m.tree.IsReviewed("c.go"))
	assert.False(t, m.tree.IsChangedSinceReview("c.go"), "never-reviewed file is plain")
	assert.Equal(t, "Resumed session for feature · 1 reviewed · 1 changed since review", m.transientHint())
	assert.Empty(t, store.saves, "loading alone writes nothing")

	// the hint clears on the next key
	m, _ = pressRune(t, m, 'j')
	assert.Empty(t, m.session.hint)
	assert.Contains(t, m.statusBarText(), "↻ 1")
}

func TestSession_FingerprintMismatchNote(t *testing.T) {
	store := newFakeSessionStore()
	store.mismatch = true
	store.byBranch["feature"] = &session.Session{ID: "s1", Branch: "feature", Name: "deep", Reviewed: map[string]string{"a.go": "v0"}}
	m := sessionModel(t, store, []string{"a.go", "b.go"}, ModelConfig{SessionName: "deep"})
	m = loadAll(t, m)
	assert.True(t, m.tree.IsChangedSinceReview("a.go"))
	assert.Equal(t, "Resumed session “deep” for feature · 1 reviewed marks from another revdiff version could not be verified", m.transientHint())
}

func TestSession_NewSessionStartupNote(t *testing.T) {
	store := newFakeSessionStore()
	store.byBranch["feature"] = &session.Session{ID: "s1", Branch: "feature", Reviewed: map[string]string{"a.go": sessionTestFingerprint("a.go")}}
	m := sessionModel(t, store, []string{"a.go", "b.go"}, ModelConfig{NewSession: true})
	assert.True(t, store.opens[0].Fresh)
	m = loadAll(t, m)
	assert.False(t, m.tree.IsReviewed("a.go"))
	assert.Equal(t, "Started a new session for feature (previous sessions are kept)", m.transientHint())

	t.Run("no note when the branch had no session", func(t *testing.T) {
		m := sessionModel(t, newFakeSessionStore(), []string{"a.go", "b.go"}, ModelConfig{})
		m = loadAll(t, m)
		assert.Empty(t, m.transientHint())
	})
}

func TestSession_MarkReviewedSavesAndMergesNarrowedRun(t *testing.T) {
	store := newFakeSessionStore()
	store.byBranch["feature"] = &session.Session{ID: "s1", Branch: "feature", Reviewed: map[string]string{
		"outside.go": "kept-even-though-not-in-this-run",
		"b.go":       "approved-before-change",
	}}
	m := sessionModel(t, store, []string{"a.go", "b.go"}, ModelConfig{})
	m = loadAll(t, m)
	require.Equal(t, "a.go", m.file.name)
	require.True(t, m.tree.IsChangedSinceReview("b.go"))

	m, _ = feed(t, m, tea.KeyMsg{Type: tea.KeySpace, Runes: []rune{' '}})
	require.True(t, m.tree.IsReviewed("a.go"))
	saved := store.lastSave(t)
	assert.Equal(t, map[string]string{
		"a.go":       sessionTestFingerprint("a.go"),
		"b.go":       "approved-before-change",
		"outside.go": "kept-even-though-not-in-this-run",
	}, saved.Reviewed, "absent paths and changed-since-review fingerprints survive the merge")

	m, _ = feed(t, m, tea.KeyMsg{Type: tea.KeySpace, Runes: []rune{' '}})
	require.False(t, m.tree.IsReviewed("a.go"))
	assert.NotContains(t, store.lastSave(t).Reviewed, "a.go", "unmarking removes the path")
}

func TestSession_AsyncMarkSaves(t *testing.T) {
	store := newFakeSessionStore()
	m := sessionModel(t, store, []string{"a.go", "b.go"}, ModelConfig{})
	m = loadAll(t, m)
	m.tree.SelectByPath("b.go")
	m.layout.focus = paneTree
	m, cmd := feed(t, m, tea.KeyMsg{Type: tea.KeySpace, Runes: []rune{' '}})
	require.NotNil(t, cmd, "b.go needs an async fingerprint")
	assert.Empty(t, store.saves)
	m = settle(t, m, cmd)
	assert.True(t, m.tree.IsReviewed("b.go"))
	assert.Equal(t, sessionTestFingerprint("b.go"), store.lastSave(t).Reviewed["b.go"])
}

func TestSession_SaveFailureHint(t *testing.T) {
	store := newFakeSessionStore()
	store.saveErr = errors.New("disk full")
	m := sessionModel(t, store, []string{"a.go", "b.go"}, ModelConfig{})
	m = loadAll(t, m)
	m, _ = feed(t, m, tea.KeyMsg{Type: tea.KeySpace, Runes: []rune{' '}})
	assert.True(t, m.tree.IsReviewed("a.go"), "the review continues")
	assert.Equal(t, "Saving the review session failed", m.transientHint())
}

func TestSession_OpenFailureDisablesPersistence(t *testing.T) {
	store := newFakeSessionStore()
	store.openErr = errors.New("boom")
	m := sessionModel(t, store, []string{"a.go", "b.go"}, ModelConfig{})
	assert.Equal(t, "Review session unavailable", m.transientHint())
	m = loadAll(t, m)
	m, _ = feed(t, m, tea.KeyMsg{Type: tea.KeySpace, Runes: []rune{' '}})
	assert.True(t, m.tree.IsReviewed("a.go"))
	assert.Empty(t, store.saves)
}

func TestSession_NewSessionAction(t *testing.T) {
	ctrlN := tea.KeyMsg{Type: tea.KeyCtrlN}

	t.Run("unavailable without sessions", func(t *testing.T) {
		m := sessionModel(t, nil, []string{"a.go", "b.go"}, ModelConfig{})
		m = loadAll(t, m)
		m, _ = feed(t, m, ctrlN)
		assert.Equal(t, "Review sessions are not available in this mode", m.transientHint())
	})

	t.Run("confirm then start fresh on the same branch", func(t *testing.T) {
		store := newFakeSessionStore()
		store.byBranch["feature"] = &session.Session{ID: "s1", Branch: "feature", Reviewed: map[string]string{"a.go": sessionTestFingerprint("a.go")}}
		m := sessionModel(t, store, []string{"a.go", "b.go"}, ModelConfig{Ref: "main...feature"})
		store.opens = nil
		m = loadAll(t, m)
		require.True(t, m.tree.IsReviewed("a.go"))
		m.store.Add(annotation.Annotation{File: "a.go", Line: 1, Type: "+", Comment: "fix"})

		m, _ = feed(t, m, ctrlN)
		assert.True(t, m.session.confirmNew)
		assert.Contains(t, m.transientHint(), "press y to confirm")

		m, _ = pressRune(t, m, 'n')
		assert.False(t, m.session.confirmNew)
		assert.Equal(t, "New session canceled", m.transientHint())
		assert.True(t, m.tree.IsReviewed("a.go"))

		m, _ = feed(t, m, ctrlN)
		m, _ = pressRune(t, m, 'y')
		require.Len(t, store.opens, 1)
		assert.Equal(t, session.Request{Ref: "main...feature", Branch: "feature", Fresh: true}, store.opens[0])
		assert.False(t, m.tree.IsReviewed("a.go"))
		assert.Equal(t, 0, m.store.Count())
		assert.Equal(t, "new", m.session.cur.ID)
		assert.Equal(t, "Started a new session for feature (previous sessions are kept)", m.transientHint())

		// the new session saves only what is marked from now on
		_, _ = feed(t, m, tea.KeyMsg{Type: tea.KeySpace, Runes: []rune{' '}})
		saved := store.lastSave(t)
		assert.Equal(t, "new", saved.ID)
		assert.Equal(t, map[string]string{"a.go": sessionTestFingerprint("a.go")}, saved.Reviewed)
	})

	t.Run("no confirmation when there is nothing to leave behind", func(t *testing.T) {
		store := newFakeSessionStore()
		m := sessionModel(t, store, []string{"a.go", "b.go"}, ModelConfig{})
		m = loadAll(t, m)
		m, _ = feed(t, m, ctrlN)
		assert.False(t, m.session.confirmNew)
		assert.True(t, store.opens[len(store.opens)-1].Fresh)
	})

	t.Run("mouse is swallowed while confirming", func(t *testing.T) {
		store := newFakeSessionStore()
		store.byBranch["feature"] = &session.Session{ID: "s1", Branch: "feature", Reviewed: map[string]string{"a.go": sessionTestFingerprint("a.go")}}
		m := sessionModel(t, store, []string{"a.go", "b.go"}, ModelConfig{})
		m = loadAll(t, m)
		m, _ = feed(t, m, ctrlN)
		require.True(t, m.session.confirmNew)
		m, _ = feed(t, m, tea.MouseMsg{X: 2, Y: 3, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress})
		assert.True(t, m.session.confirmNew)
	})
}

func TestSession_RefSwitchOpensTargetSession(t *testing.T) {
	store := newFakeSessionStore()
	store.byBranch["widget"] = &session.Session{ID: "w1", Branch: "widget", Reviewed: map[string]string{"a.go": "stale"}}
	m, fx := refSwitchModel(t, ModelConfig{Sessions: store})
	require.Len(t, store.opens, 1)

	t.Run("pull request uses its head branch", func(t *testing.T) {
		sm := openSwitcher(t, m)
		sm, cmd := typeAndEnter(t, sm, "widget")
		require.NotNil(t, cmd)
		sm, cmd = feed(t, sm, cmd())
		require.Len(t, store.opens, 2)
		assert.Equal(t, session.Request{Ref: "aaa...bbb", Branch: "widget"}, store.opens[1])
		assert.Equal(t, "w1", sm.session.cur.ID)
		assert.Nil(t, sm.session.present, "present paths belong to the previous diff")
		sm = settle(t, sm, cmd)
		assert.True(t, sm.tree.IsChangedSinceReview("a.go"))
		assert.Equal(t, "Resumed session for widget · 0 reviewed · 1 changed since review", sm.transientHint())
	})

	t.Run("branch uses its name", func(t *testing.T) {
		store.opens = store.opens[:1]
		sm := openSwitcher(t, m)
		_, cmd := typeAndEnter(t, sm, "feature")
		require.NotNil(t, cmd)
		require.Len(t, store.opens, 2)
		assert.Equal(t, session.Request{Ref: "origin/main...feature", Branch: "feature"}, store.opens[1])
	})
	_ = fx
}
