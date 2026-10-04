package ui

import (
	"errors"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/umputun/revdiff/app/annotation"
	"github.com/umputun/revdiff/app/diff"
	"github.com/umputun/revdiff/app/refsource"
	"github.com/umputun/revdiff/app/ui/mocks"
	"github.com/umputun/revdiff/app/ui/overlay"
)

type refSwitchFixture struct {
	src        *mocks.RefSourceMock
	commits    *fakeCommitLog
	changedRef []string
}

func newRefSource() *mocks.RefSourceMock {
	return &mocks.RefSourceMock{
		BranchesFunc: func() (refsource.BranchList, error) {
			return refsource.BranchList{Base: "origin/main", Branches: []refsource.Branch{
				{Name: "feature", Ref: "origin/main...feature", Current: true, Age: "2 days ago", Subject: "add\x1b[31m thing"},
				{Name: "origin/main", Ref: ""},
			}}, nil
		},
		PullRequestsFunc: func() ([]refsource.PullRequest, error) {
			return []refsource.PullRequest{{Number: 12, Title: "Add\nwidget", Head: "widget", Base: "main", Author: "alice", Draft: true}}, nil
		},
		PullRequestRefFunc: func(n int) (string, error) { return "aaa...bbb", nil },
		CheckRefFunc:       func(string) error { return nil },
	}
}

// refSwitchModel builds a working-tree model with a ref source, a commit log
// source that the composition root ruled not applicable (working tree), and
// untracked display on — the startup state the switcher must restore.
func refSwitchModel(t *testing.T, cfg ModelConfig) (Model, *refSwitchFixture) {
	t.Helper()
	fx := &refSwitchFixture{src: newRefSource(), commits: &fakeCommitLog{}}
	renderer := &mocks.RendererMock{
		ChangedFilesFunc: func(ref string, staged bool) ([]diff.FileEntry, error) {
			fx.changedRef = append(fx.changedRef, ref)
			return []diff.FileEntry{{Path: "a.go"}}, nil
		},
		FileDiffFunc: func(diff.FileDiffRequest) ([]diff.DiffLine, error) { return nil, nil },
	}
	if cfg.RefSource == nil {
		cfg.RefSource = fx.src
	}
	cfg.CommitLog = fx.commits
	cfg.LoadUntracked = func() ([]string, error) { return nil, nil }
	cfg.ShowUntracked = true
	cfg.SourceEditor = SourceEditorPolicy{Available: true, Root: "/repo", ReloadAfterCleanExit: true, DisallowAnnotatedFileEditing: true}
	cfg.ReviewInfo = &ReviewInfoConfig{VCS: "git", WorkDir: "/repo"}
	m := testNewModel(t, renderer, annotation.NewStore(), noopHighlighter(), cfg)
	m.layout.width, m.layout.height = 120, 40
	m.ready, m.filesLoaded = true, true
	return m, fx
}

func pressRune(t *testing.T, m Model, r rune) (Model, tea.Cmd) {
	t.Helper()
	res, cmd := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}})
	return res.(Model), cmd
}

func feed(t *testing.T, m Model, msg tea.Msg) (Model, tea.Cmd) {
	t.Helper()
	res, cmd := m.Update(msg)
	return res.(Model), cmd
}

// openSwitcher presses b and delivers both list loads.
func openSwitcher(t *testing.T, m Model) Model {
	t.Helper()
	m, cmd := pressRune(t, m, 'b')
	require.NotNil(t, cmd)
	require.Equal(t, overlay.KindRefPicker, m.overlay.Kind())
	batch, ok := cmd().(tea.BatchMsg)
	require.True(t, ok)
	for _, c := range batch {
		m, _ = feed(t, m, c())
	}
	return m
}

// typeAndEnter filters the switcher and confirms the cursor row.
func typeAndEnter(t *testing.T, m Model, text string) (Model, tea.Cmd) {
	t.Helper()
	for _, r := range text {
		m, _ = pressRune(t, m, r)
	}
	return feed(t, m, tea.KeyMsg{Type: tea.KeyEnter})
}

func TestRefSwitch_UnavailableWithoutSource(t *testing.T) {
	m := testNewModel(t, plainRenderer(), annotation.NewStore(), noopHighlighter(), ModelConfig{})
	m, cmd := pressRune(t, m, 'b')
	assert.Nil(t, cmd)
	assert.False(t, m.overlay.Active())
	assert.Equal(t, "Switching review is not available in this mode", m.transientHint())
}

func TestRefSwitch_OpenListsSections(t *testing.T) {
	m, _ := refSwitchModel(t, ModelConfig{})
	m, cmd := pressRune(t, m, 'b')
	require.NotNil(t, cmd)
	spec := m.buildRefPickerSpec()
	assert.True(t, spec.Loading, "lists not yet loaded")
	require.Len(t, spec.Items, 1)
	assert.Equal(t, "working tree changes", spec.Items[0].Label)

	batch, ok := cmd().(tea.BatchMsg)
	require.True(t, ok)
	for _, c := range batch {
		m, _ = feed(t, m, c())
	}
	spec = m.buildRefPickerSpec()
	assert.False(t, spec.Loading)
	assert.Equal(t, "working tree changes", spec.Current)
	assert.Equal(t, refIDOriginal, spec.ActiveID)
	require.Len(t, spec.Items, 4)
	assert.Equal(t, overlay.RefItem{ID: "pr:12", Section: "pull requests", Label: "#12 Add widget", Detail: "widget → main · @alice · draft"}, spec.Items[1])
	assert.Equal(t, overlay.RefItem{ID: "branch:feature", Section: "branches", Label: "feature",
		Detail: "current · vs origin/main · 2 days ago · add thing"}, spec.Items[2], "control sequences stripped")
	assert.Equal(t, "base branch", spec.Items[3].Detail)
}

func TestRefSwitch_ListErrorsBecomeNotices(t *testing.T) {
	src := newRefSource()
	src.PullRequestsFunc = func() ([]refsource.PullRequest, error) { return nil, refsource.ErrGHNotFound }
	src.BranchesFunc = func() (refsource.BranchList, error) { return refsource.BranchList{}, errors.New("boom\nmore") }
	m, _ := refSwitchModel(t, ModelConfig{RefSource: src})
	m = openSwitcher(t, m)
	spec := m.buildRefPickerSpec()
	assert.Equal(t, []string{"branches unavailable: boom more", "pull requests unavailable: gh CLI not found on PATH"}, spec.Notices)
	assert.Len(t, spec.Items, 1, "original stays selectable")
}

func TestRefSwitch_StaleListsDropped(t *testing.T) {
	m, _ := refSwitchModel(t, ModelConfig{})
	m, _ = pressRune(t, m, 'b')
	m, _ = feed(t, m, refBranchesLoadedMsg{seq: m.refs.listSeq - 1, list: refsource.BranchList{Base: "x"}})
	assert.False(t, m.refs.branchesLoaded)
	m, _ = feed(t, m, refPullRequestsLoadedMsg{seq: m.refs.listSeq - 1})
	assert.False(t, m.refs.prsLoaded)
}

func TestRefSwitch_BranchSwitchesToThreeDotRange(t *testing.T) {
	m, fx := refSwitchModel(t, ModelConfig{})
	m = openSwitcher(t, m)
	m, cmd := typeAndEnter(t, m, "feature")
	require.NotNil(t, cmd)
	assert.False(t, m.overlay.Active())

	ref, staged := m.ReviewRef()
	assert.Equal(t, "origin/main...feature", ref)
	assert.False(t, staged)
	assert.False(t, m.modes.showUntracked, "untracked files are working-tree state, hidden for a range")
	assert.True(t, m.commits.applicable, "a ref review lists its commits")
	assert.False(t, m.cfg.sourceEditorPolicy.ReloadAfterCleanExit)
	assert.False(t, m.cfg.sourceEditorPolicy.DisallowAnnotatedFileEditing)
	assert.True(t, m.cfg.sourceEditorPolicy.Available)
	assert.False(t, m.filesLoaded, "reload in flight")
	assert.Equal(t, "Reviewing branch feature", m.transientHint())
	assert.Equal(t, "branch feature", m.reviewHeaderText())
	assert.Equal(t, overlay.InfoRow{Label: "ref", Value: "origin/main...feature"}, m.reviewRows()[0])
	assert.Equal(t, "origin/main...feature", m.review.cfg.Ref)
	assert.Empty(t, m.refs.origin.reviewCfg.Ref, "startup review config is not mutated")

	batch, ok := cmd().(tea.BatchMsg)
	require.True(t, ok)
	for _, c := range batch {
		c()
	}
	assert.Equal(t, []string{"origin/main...feature"}, fx.changedRef)
	assert.Equal(t, "origin/main...feature", fx.commits.lastRef, "commit log follows the switched ref")
}

func TestRefSwitch_MouseClickSwitches(t *testing.T) {
	m, _ := refSwitchModel(t, ModelConfig{})
	m = openSwitcher(t, m)
	m, _ = feed(t, m, filesLoadedMsg{seq: m.filesLoadSeq, entries: []diff.FileEntry{{Path: "a.go"}}})
	// filter to one branch so its row index is known: header at row 0, entry at row 1
	for _, r := range "feature" {
		m, _ = pressRune(t, m, r)
	}
	view := m.View() // rendering records the popup bounds used for click hit-testing
	require.Contains(t, view, "feature")
	x, y := -1, -1
	for i, line := range strings.Split(ansi.Strip(view), "\n") {
		if before, _, ok := strings.Cut(line, "> feature"); ok {
			x, y = lipgloss.Width(before), i
			break
		}
	}
	require.GreaterOrEqual(t, y, 0, "selected branch row rendered")
	m, cmd := feed(t, m, tea.MouseMsg{X: x + 2, Y: y, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress})
	require.NotNil(t, cmd)
	assert.Equal(t, "origin/main...feature", m.cfg.ref)
}

func TestRefSwitch_BaseBranchIsNotSwitchable(t *testing.T) {
	m, _ := refSwitchModel(t, ModelConfig{})
	m = openSwitcher(t, m)
	m, cmd := typeAndEnter(t, m, "base branch")
	assert.Nil(t, cmd)
	assert.Empty(t, m.cfg.ref)
	assert.Contains(t, m.transientHint(), "origin/main is the base branch")
}

func TestRefSwitch_PullRequestFetchesAsync(t *testing.T) {
	m, fx := refSwitchModel(t, ModelConfig{})
	m = openSwitcher(t, m)
	m, cmd := typeAndEnter(t, m, "#12")
	require.NotNil(t, cmd)
	assert.Empty(t, m.cfg.ref, "nothing switches before the fetch lands")
	assert.Equal(t, "Fetching PR #12…", m.transientHint())

	msg := cmd()
	require.Len(t, fx.src.PullRequestRefCalls(), 1)
	assert.Equal(t, 12, fx.src.PullRequestRefCalls()[0].Number)
	m, cmd = feed(t, m, msg)
	require.NotNil(t, cmd)
	assert.Equal(t, "aaa...bbb", m.cfg.ref)
	assert.Equal(t, "PR #12", m.reviewHeaderText())
	assert.Equal(t, "pr:12", m.refs.activeID)
	assert.Equal(t, "PR #12", m.buildRefPickerSpec().Current)
}

func TestRefSwitch_PullRequestFailureKeepsReview(t *testing.T) {
	src := newRefSource()
	src.PullRequestRefFunc = func(int) (string, error) { return "", errors.New("fetch PR #12: denied") }
	m, _ := refSwitchModel(t, ModelConfig{RefSource: src})
	m = openSwitcher(t, m)
	m, cmd := typeAndEnter(t, m, "#12")
	m, cmd = feed(t, m, cmd())
	assert.Nil(t, cmd)
	assert.Empty(t, m.cfg.ref)
	assert.Equal(t, "Switch failed: fetch PR #12: denied", m.transientHint())
}

func TestRefSwitch_StaleResolutionDropped(t *testing.T) {
	m, _ := refSwitchModel(t, ModelConfig{})
	m, cmd := feed(t, m, refResolvedMsg{seq: m.refs.resolveSeq + 5, target: reviewTarget{ref: "x...y"}})
	assert.Nil(t, cmd)
	assert.Empty(t, m.cfg.ref)
}

func TestRefSwitch_TypedRef(t *testing.T) {
	t.Run("valid ref switches without a label", func(t *testing.T) {
		m, fx := refSwitchModel(t, ModelConfig{})
		m = openSwitcher(t, m)
		m, cmd := typeAndEnter(t, m, "HEAD~3")
		require.NotNil(t, cmd)
		assert.Equal(t, "Checking HEAD~3…", m.transientHint())
		m, cmd = feed(t, m, cmd())
		require.NotNil(t, cmd)
		require.Len(t, fx.src.CheckRefCalls(), 1)
		assert.Equal(t, "HEAD~3", fx.src.CheckRefCalls()[0].Ref)
		assert.Equal(t, "HEAD~3", m.cfg.ref)
		assert.True(t, m.modes.showUntracked, "a single ref keeps the untracked toggle, like startup")
		assert.Equal(t, "changes against HEAD~3", m.reviewHeaderText())
		assert.Empty(t, m.refs.activeID)
	})

	t.Run("invalid ref keeps review", func(t *testing.T) {
		src := newRefSource()
		src.CheckRefFunc = func(string) error { return errors.New(`unknown ref "nope"`) }
		m, _ := refSwitchModel(t, ModelConfig{RefSource: src})
		m = openSwitcher(t, m)
		m, cmd := typeAndEnter(t, m, "nope")
		m, _ = feed(t, m, cmd())
		assert.Empty(t, m.cfg.ref)
		assert.Equal(t, `Switch failed: unknown ref "nope"`, m.transientHint())
	})
}

func TestRefSwitch_AnnotationsRequireConfirmation(t *testing.T) {
	setup := func(t *testing.T, cfg ModelConfig) Model {
		m, _ := refSwitchModel(t, cfg)
		m.store.Add(annotation.Annotation{File: "a.go", Line: 1, Type: "+", Comment: "note"})
		m = openSwitcher(t, m)
		m, cmd := typeAndEnter(t, m, "feature")
		if cfg.NoConfirmReload {
			require.NotNil(t, cmd)
		} else {
			assert.Nil(t, cmd)
		}
		return m
	}

	t.Run("other key cancels", func(t *testing.T) {
		m := setup(t, ModelConfig{})
		require.NotNil(t, m.refs.pending)
		assert.Equal(t, "Annotations will be dropped — press y to switch to branch feature, any other key to cancel", m.transientHint())
		m, cmd := pressRune(t, m, 'n')
		assert.Nil(t, cmd)
		assert.Nil(t, m.refs.pending)
		assert.Empty(t, m.cfg.ref)
		assert.Equal(t, 1, m.store.Count())
		assert.Equal(t, "Switch canceled", m.transientHint())
	})

	t.Run("mouse is swallowed while the prompt is pending", func(t *testing.T) {
		m := setup(t, ModelConfig{})
		m, cmd := feed(t, m, tea.MouseMsg{X: 5, Y: 5, Button: tea.MouseButtonLeft, Action: tea.MouseActionPress})
		assert.Nil(t, cmd)
		require.NotNil(t, m.refs.pending)
		assert.Contains(t, m.transientHint(), "press y to switch")
	})

	t.Run("y switches and clears annotations", func(t *testing.T) {
		m := setup(t, ModelConfig{})
		m, cmd := pressRune(t, m, 'y')
		require.NotNil(t, cmd)
		assert.Equal(t, "origin/main...feature", m.cfg.ref)
		assert.Zero(t, m.store.Count())
	})

	t.Run("no-confirm-reload skips the prompt", func(t *testing.T) {
		m := setup(t, ModelConfig{NoConfirmReload: true})
		assert.Equal(t, "origin/main...feature", m.cfg.ref)
		assert.Zero(t, m.store.Count())
	})
}

func TestRefSwitch_OriginalRestoresStartupState(t *testing.T) {
	m, _ := refSwitchModel(t, ModelConfig{})
	startCommits := m.commits.applicable
	startReview := m.review.cfg
	m = openSwitcher(t, m)
	m, _ = typeAndEnter(t, m, "feature")
	require.Equal(t, "origin/main...feature", m.cfg.ref)

	m = openSwitcher(t, m)
	assert.Equal(t, "branch:feature", m.buildRefPickerSpec().ActiveID)
	m, cmd := typeAndEnter(t, m, "started with")
	require.NotNil(t, cmd)
	ref, staged := m.ReviewRef()
	assert.Empty(t, ref)
	assert.False(t, staged)
	assert.True(t, m.modes.showUntracked)
	assert.Equal(t, startCommits, m.commits.applicable)
	assert.True(t, m.cfg.sourceEditorPolicy.ReloadAfterCleanExit)
	assert.True(t, m.cfg.sourceEditorPolicy.DisallowAnnotatedFileEditing)
	assert.Same(t, startReview, m.review.cfg)
	assert.Equal(t, "working tree changes", m.reviewHeaderText())
	assert.Equal(t, refIDOriginal, m.refs.activeID)
	assert.Equal(t, "Reviewing working tree changes", m.transientHint())

	// choosing the review already shown is a no-op
	m = openSwitcher(t, m)
	m, cmd = typeAndEnter(t, m, "started with")
	assert.Nil(t, cmd)
	assert.Equal(t, "Already reviewing working tree changes", m.transientHint())
}

func TestRefSwitch_StagedOriginLabel(t *testing.T) {
	m, _ := refSwitchModel(t, ModelConfig{Staged: true})
	assert.Equal(t, "staged changes", m.originLabel())
	m = openSwitcher(t, m)
	m, _ = typeAndEnter(t, m, "feature")
	_, staged := m.ReviewRef()
	assert.False(t, staged, "a switched review is never staged")
	assert.False(t, m.review.cfg.Staged)
}
