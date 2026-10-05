package ui

import (
	"errors"
	"maps"
	"slices"
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
	cp.Annotations = slices.Clone(s.Annotations)
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
	return sessionModelWith(t, store, sessionTestDiffs, files, cfg)
}

// sessionModelWith is sessionModel over caller-owned diffs; the renderer reads
// the map on every call, so a test changes "the code" by assigning to it.
func sessionModelWith(t *testing.T, store *fakeSessionStore, diffs map[string][]diff.DiffLine, files []string, cfg ModelConfig) Model {
	t.Helper()
	entries := make([]diff.FileEntry, 0, len(files))
	for _, f := range files {
		entries = append(entries, diff.FileEntry{Path: f})
	}
	renderer := &mocks.RendererMock{
		ChangedFilesFunc: func(string, bool) ([]diff.FileEntry, error) { return entries, nil },
		FileDiffFunc: func(req diff.FileDiffRequest) ([]diff.DiffLine, error) {
			return diffs[req.Path], nil
		},
	}
	if store != nil {
		cfg.Sessions = store
	}
	st := cfg.Store
	if st == nil {
		st = annotation.NewStore()
	}
	m := testNewModel(t, renderer, st, noopHighlighter(), cfg)
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

// annotationDiffs is a two-file review whose a.go changes between rounds.
func annotationDiffs() map[string][]diff.DiffLine {
	return map[string][]diff.DiffLine{
		"a.go": {
			{NewNum: 1, Content: "package a", ChangeType: diff.ChangeContext, OldNum: 1},
			{NewNum: 2, Content: "func A() int {", ChangeType: diff.ChangeAdd},
			{NewNum: 3, Content: "\treturn compute(1)", ChangeType: diff.ChangeAdd},
			{NewNum: 4, Content: "}", ChangeType: diff.ChangeAdd},
		},
		"b.go": {{NewNum: 1, Content: "package b", ChangeType: diff.ChangeAdd}},
	}
}

// anchoredAnnotation builds an a.go annotation on diffs["a.go"][idx] with its anchor.
func anchoredAnnotation(t *testing.T, diffs map[string][]diff.DiffLine, idx int, comment string) annotation.Annotation {
	t.Helper()
	dl := diffs["a.go"][idx]
	return annotation.Annotation{File: "a.go", Line: dl.NewNum, Type: string(dl.ChangeType), Comment: comment,
		Anchor: annotation.NewAnchor(diffs["a.go"], idx)}
}

func TestSession_ResumeReanchorsAnnotations(t *testing.T) {
	diffs := annotationDiffs()
	store := newFakeSessionStore()
	sent := anchoredAnnotation(t, diffs, 2, "use a constant")
	sent.Delivered = true
	kept := anchoredAnnotation(t, diffs, 1, "name it better")
	gone := annotation.Annotation{File: "deleted.go", Line: 3, Type: "+", Comment: "file left the diff"}
	store.byBranch["feature"] = &session.Session{ID: "s1", Branch: "feature", Reviewed: map[string]string{},
		Annotations: []annotation.Annotation{sent, kept, gone}}

	// the agent addressed the delivered comment and inserted a doc line above the function
	diffs["a.go"] = []diff.DiffLine{
		{NewNum: 1, OldNum: 1, Content: "package a", ChangeType: diff.ChangeContext},
		{NewNum: 2, Content: "// A computes.", ChangeType: diff.ChangeAdd},
		{NewNum: 3, Content: "func A() int {", ChangeType: diff.ChangeAdd},
		{NewNum: 4, Content: "\treturn compute(answer)", ChangeType: diff.ChangeAdd},
		{NewNum: 5, Content: "}", ChangeType: diff.ChangeAdd},
	}
	m := sessionModelWith(t, store, diffs, []string{"a.go", "b.go"}, ModelConfig{})
	assert.Equal(t, 3, m.store.Count(), "the session's annotations are loaded at open")
	m = loadAll(t, m)

	got := m.store.Get("a.go")
	require.Len(t, got, 2)
	assert.Equal(t, "use a constant", got[0].Comment)
	assert.Equal(t, annotation.StatusOutdated, got[0].Status, "the edited line's annotation is outdated")
	assert.Equal(t, -1, got[0].Line, "its old line 3 is now taken by the moved annotation, so it is detached")
	assert.True(t, got[0].Delivered)
	assert.Equal(t, 3, got[1].Line, "the unchanged line moved down with the insertion")
	assert.Equal(t, annotation.StatusOpen, got[1].Status)
	assert.Equal(t, "name it better", got[1].Comment)
	require.Len(t, m.store.Get("deleted.go"), 1)
	assert.Equal(t, annotation.StatusOutdated, m.store.Get("deleted.go")[0].Status)

	assert.Equal(t, "## a.go:3 (+)\nname it better\n", m.store.FormatOutput(), "only the pending annotation is output")
	assert.Equal(t, "Resumed session for feature · 0 reviewed · 3 annotations · 2 outdated", m.transientHint())
	saved := store.lastSave(t)
	assert.Len(t, saved.Annotations, 3, "re-anchoring saves the session")

	assert.NotContains(t, m.renderDiff(), "use a constant", "a detached annotation renders nowhere")
}

func TestSession_PreloadedAnnotationsReplaceSession(t *testing.T) {
	store := newFakeSessionStore()
	store.byBranch["feature"] = &session.Session{ID: "s1", Branch: "feature", Reviewed: map[string]string{},
		Annotations: []annotation.Annotation{{File: "a.go", Line: 1, Type: "+", Comment: "old"}}}
	st := annotation.NewStore()
	st.Add(annotation.Annotation{File: "b.go", Line: 1, Type: "+", Comment: "from --annotations"})
	m := sessionModel(t, store, []string{"a.go", "b.go"}, ModelConfig{Store: st, PreloadedAnnotations: true})
	assert.Equal(t, []string{"b.go"}, m.store.Files())
	saved := store.lastSave(t)
	require.Len(t, saved.Annotations, 1)
	assert.Equal(t, "from --annotations", saved.Annotations[0].Comment)

	m = loadAll(t, m)
	require.NotNil(t, m.store.Get("b.go")[0].Anchor, "the first load captures an anchor for preloaded annotations")
}

func TestSession_AnnotationEditsSave(t *testing.T) {
	diffs := annotationDiffs()
	store := newFakeSessionStore()
	m := sessionModelWith(t, store, diffs, []string{"a.go", "b.go"}, ModelConfig{})
	m = loadAll(t, m)
	m.layout.focus = paneDiff
	m.nav.diffCursor = 2

	m, _ = pressRune(t, m, 'a')
	for _, r := range "fix this" {
		m, _ = pressRune(t, m, r)
	}
	m, _ = feed(t, m, tea.KeyMsg{Type: tea.KeyEnter})
	saved := store.lastSave(t)
	require.Len(t, saved.Annotations, 1)
	assert.Equal(t, "fix this", saved.Annotations[0].Comment)
	require.NotNil(t, saved.Annotations[0].Anchor)
	assert.Equal(t, "\treturn compute(1)", saved.Annotations[0].Anchor.Content)

	m.annot.cursorOnAnnotation = true
	_, _ = pressRune(t, m, 'd')
	assert.Empty(t, store.lastSave(t).Annotations, "delete saves")
}

func TestSession_ResolveAnnotation(t *testing.T) {
	diffs := annotationDiffs()
	store := newFakeSessionStore()
	m := sessionModelWith(t, store, diffs, []string{"a.go", "b.go"}, ModelConfig{})
	m = loadAll(t, m)
	m.layout.focus = paneDiff
	m.nav.diffCursor = 2

	m, _ = pressRune(t, m, 'x')
	assert.Equal(t, "No annotation on this line", m.transientHint())

	m.store.Add(anchoredAnnotation(t, diffs, 2, "use a constant"))
	m, _ = pressRune(t, m, 'x')
	got := m.store.Get("a.go")[0]
	assert.Equal(t, annotation.StatusResolved, got.Status)
	assert.Empty(t, m.store.FormatOutput(), "resolved annotations are never output")
	assert.Equal(t, annotation.StatusResolved, store.lastSave(t).Annotations[0].Status)
	assert.Contains(t, m.renderDiff(), "[resolved] use a constant")

	// a delivered annotation that is reopened is sent again
	got.Delivered = true
	m.store.Add(got)
	m, _ = pressRune(t, m, 'x')
	got = m.store.Get("a.go")[0]
	assert.Equal(t, annotation.StatusOpen, got.Status)
	assert.False(t, got.Delivered)
	assert.Contains(t, m.renderDiff(), "use a constant")
	assert.NotContains(t, m.renderDiff(), "[resolved]", "the status change repaints despite the render cache")

	t.Run("reopening an outdated annotation re-anchors it at its line", func(t *testing.T) {
		stale := annotation.Annotation{File: "a.go", Line: 3, Type: "+", Comment: "old", Status: annotation.StatusOutdated,
			Anchor: &annotation.Anchor{Line: 3, Type: "+", Content: "\treturn compute(0)"}}
		m.store.Add(stale)
		m2, _ := pressRune(t, m, 'x')
		got := m2.store.Get("a.go")[0]
		assert.Equal(t, annotation.StatusOpen, got.Status)
		assert.Equal(t, "\treturn compute(1)", got.Anchor.Content)
	})

	t.Run("file-level annotation", func(t *testing.T) {
		m.store.Add(annotation.Annotation{File: "a.go", Comment: "overall"})
		m.nav.diffCursor = -1
		m2, _ := pressRune(t, m, 'x')
		assert.Equal(t, annotation.StatusResolved, m2.store.Get("a.go")[0].Status)
	})
}

func TestSession_DeliveryAndFlush(t *testing.T) {
	diffs := annotationDiffs()
	store := newFakeSessionStore()
	out := t.TempDir() + "/out.md"
	m := sessionModelWith(t, store, diffs, []string{"a.go", "b.go"}, ModelConfig{OutputPath: out})
	m = loadAll(t, m)
	m.store.Add(anchoredAnnotation(t, diffs, 2, "first"))

	m, _ = pressRune(t, m, 'O')
	assert.Equal(t, "Wrote 1 annotation to output file", m.transientHint())
	assert.True(t, m.store.Get("a.go")[0].Delivered, "the flushed annotation is delivered")
	assert.True(t, store.lastSave(t).Annotations[0].Delivered)
	assert.Contains(t, m.renderDiff(), "[sent] first")

	m, _ = pressRune(t, m, 'O')
	assert.Equal(t, "No new annotations to flush", m.transientHint())

	m.store.Add(anchoredAnnotation(t, diffs, 1, "second"))
	m, _ = pressRune(t, m, 'O')
	assert.Equal(t, "Wrote 1 annotation to output file", m.transientHint(), "only what is new is flushed")

	m.store.Add(anchoredAnnotation(t, diffs, 3, "third"))
	m.MarkDelivered()
	assert.Equal(t, 0, m.store.PendingCount())

	t.Run("hook-only flush delivers after the command succeeds", func(t *testing.T) {
		hookStore := newFakeSessionStore()
		hm := sessionModelWith(t, hookStore, diffs, []string{"a.go", "b.go"}, ModelConfig{PostFlushHook: &postFlushHookStub{}})
		hm = loadAll(t, hm)
		hm.store.Add(anchoredAnnotation(t, diffs, 2, "hooked"))
		hm, cmd := pressRune(t, hm, 'O')
		require.NotNil(t, cmd)
		assert.False(t, hm.store.Get("a.go")[0].Delivered, "not delivered before the command ran")
		hm, _ = feed(t, hm, postFlushFinishedMsg{err: errors.New("boom"), deliver: true})
		assert.False(t, hm.store.Get("a.go")[0].Delivered, "a failed command delivers nothing")
		hm, _ = feed(t, hm, postFlushFinishedMsg{deliver: true})
		assert.True(t, hm.store.Get("a.go")[0].Delivered)
	})

	t.Run("without a session flush stays a pure export", func(t *testing.T) {
		plain := sessionModelWith(t, nil, diffs, []string{"a.go", "b.go"}, ModelConfig{OutputPath: t.TempDir() + "/o.md"})
		plain = loadAll(t, plain)
		plain.store.Add(annotation.Annotation{File: "a.go", Line: 2, Type: "+", Comment: "x"})
		plain, _ = pressRune(t, plain, 'O')
		plain, _ = pressRune(t, plain, 'O')
		assert.Equal(t, "Wrote 1 annotation to output file", plain.transientHint())
		plain.MarkDelivered()
		assert.Equal(t, 1, plain.store.PendingCount())
	})
}

func TestSession_ReloadKeepsAndReanchorsAnnotations(t *testing.T) {
	diffs := annotationDiffs()
	store := newFakeSessionStore()
	m := sessionModelWith(t, store, diffs, []string{"a.go", "b.go"}, ModelConfig{ReloadApplicable: true})
	m = loadAll(t, m)
	m.store.Add(anchoredAnnotation(t, diffs, 2, "use a constant"))

	diffs["a.go"] = []diff.DiffLine{
		{NewNum: 1, OldNum: 1, Content: "package a", ChangeType: diff.ChangeContext},
		{NewNum: 2, Content: "func A() int {", ChangeType: diff.ChangeAdd},
		{NewNum: 3, Content: "\treturn compute(answer)", ChangeType: diff.ChangeAdd},
		{NewNum: 4, Content: "}", ChangeType: diff.ChangeAdd},
	}
	m, cmd := pressRune(t, m, 'R')
	assert.False(t, m.reload.pending, "no confirmation: annotations are kept")
	require.NotNil(t, cmd)
	m = settle(t, m, cmd)
	got := m.store.Get("a.go")
	require.Len(t, got, 1)
	assert.Equal(t, annotation.StatusOutdated, got[0].Status)
	assert.Equal(t, 3, got[0].Line, "shown at its old line, which still exists")
	m.file.name = ""
	m = settle(t, m, m.requestFileDiff("a.go"))
	rendered := m.renderDiff()
	assert.Contains(t, rendered, "[outdated] use a constant")
	assert.Contains(t, rendered, "\033[2m", "outdated rows are dimmed")
	assert.Empty(t, m.store.FormatOutput())
}

func TestSession_EditedDuringLoadIsNotOverwritten(t *testing.T) {
	diffs := annotationDiffs()
	store := newFakeSessionStore()
	m := sessionModelWith(t, store, diffs, []string{"a.go", "b.go"}, ModelConfig{})
	m.store.Add(annotation.Annotation{File: "a.go", Line: 3, Type: "+", Comment: "before load",
		Anchor: &annotation.Anchor{Line: 3, Type: "+", Content: "gone"}})
	msg := m.loadFiles()()
	m.store.Add(annotation.Annotation{File: "a.go", Line: 3, Type: "+", Comment: "typed while loading"})
	m, _ = feed(t, m, msg)
	got := m.store.Get("a.go")
	require.Len(t, got, 1)
	assert.Equal(t, "typed while loading", got[0].Comment)
	assert.Equal(t, annotation.StatusOpen, got[0].Status, "the stale re-anchor result is dropped")
}

func TestSession_RefSwitchKeepsAnnotationsInTheirSession(t *testing.T) {
	store := newFakeSessionStore()
	store.byBranch["widget"] = &session.Session{ID: "w1", Branch: "widget", Reviewed: map[string]string{},
		Annotations: []annotation.Annotation{{File: "a.go", Line: 9, Type: "+", Comment: "on widget"}}}
	m, _ := refSwitchModel(t, ModelConfig{Sessions: store})
	m.store.Add(annotation.Annotation{File: "a.go", Line: 1, Type: "+", Comment: "on feature"})

	m = openSwitcher(t, m)
	m, cmd := typeAndEnter(t, m, "widget")
	require.NotNil(t, cmd)
	m, _ = feed(t, m, cmd())
	assert.Nil(t, m.refs.pending, "no drop confirmation: the annotations stay in their session")
	assert.Equal(t, "on widget", m.store.Get("a.go")[0].Comment)
}

func TestSession_DiscardQuitDropsOnlyPending(t *testing.T) {
	diffs := annotationDiffs()
	store := newFakeSessionStore()
	m := sessionModelWith(t, store, diffs, []string{"a.go", "b.go"}, ModelConfig{})
	m = loadAll(t, m)
	sent := anchoredAnnotation(t, diffs, 2, "sent earlier")
	sent.Delivered = true
	m.store.Add(sent)
	m.store.Add(anchoredAnnotation(t, diffs, 1, "draft"))

	m, _ = pressRune(t, m, 'Q')
	require.True(t, m.inConfirmDiscard)
	assert.Equal(t, "discard 1 annotations? [y/n]", m.statusBarText())
	m, cmd := pressRune(t, m, 'y')
	require.NotNil(t, cmd)
	assert.True(t, m.Discarded())
	saved := store.lastSave(t)
	require.Len(t, saved.Annotations, 1)
	assert.Equal(t, "sent earlier", saved.Annotations[0].Comment)

	t.Run("nothing pending quits without asking", func(t *testing.T) {
		m2 := sessionModelWith(t, newFakeSessionStore(), diffs, []string{"a.go", "b.go"}, ModelConfig{})
		m2.store.Add(sent)
		m2, _ = pressRune(t, m2, 'Q')
		assert.False(t, m2.inConfirmDiscard)
		assert.True(t, m2.Discarded())
	})
}

func TestSession_AnnotationListShowsStatus(t *testing.T) {
	m := sessionModel(t, newFakeSessionStore(), []string{"a.go", "b.go"}, ModelConfig{})
	m.store.Add(annotation.Annotation{File: "a.go", Line: 1, Type: "+", Comment: "a", Delivered: true})
	m.store.Add(annotation.Annotation{File: "a.go", Line: 2, Type: "+", Comment: "b", Status: annotation.StatusResolved})
	m.store.Add(annotation.Annotation{File: "a.go", Line: -1, Type: "+", Comment: "c", Status: annotation.StatusOutdated,
		Anchor: &annotation.Anchor{Line: 7, Type: "+"}})
	m.store.Add(annotation.Annotation{File: "a.go", Line: 3, Type: "+", Comment: "d"})
	items := m.buildAnnotListSpec().Items
	require.Len(t, items, 4)
	assert.Equal(t, "outdated", items[0].Status)
	assert.Equal(t, 7, items[0].WasLine)
	assert.Equal(t, "sent", items[1].Status)
	assert.Equal(t, "resolved", items[2].Status)
	assert.Empty(t, items[3].Status)
	assert.Contains(t, m.statusBarText(), "1 outdated")
}
