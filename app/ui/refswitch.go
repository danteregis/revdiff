package ui

//go:generate moq -out mocks/ref_source.go -pkg mocks -skip-ensure -fmt goimports . RefSource

import (
	"fmt"
	"strconv"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/umputun/revdiff/app/diff"
	"github.com/umputun/revdiff/app/refsource"
	"github.com/umputun/revdiff/app/session"
	"github.com/umputun/revdiff/app/ui/overlay"
)

// RefSource is what Model needs to switch the reviewed diff at runtime: list
// local branches and open pull requests, fetch a pull request into a
// GitHub-equivalent three-dot ref, and validate a typed ref. Implemented by
// *refsource.Source (git + gh). Wired through ModelConfig.RefSource only for
// git diffs; nil disables the switcher.
type RefSource interface {
	Branches() (refsource.BranchList, error)
	PullRequests() ([]refsource.PullRequest, error)
	PullRequestRef(number int) (string, error)
	CheckRef(ref string) error
}

const (
	refIDOriginal     = "original"
	refIDBranchPrefix = "branch:"
	refIDPRPrefix     = "pr:"
)

// reviewTarget is a resolved switcher selection: the ref (and staged flag) to
// review, plus how to present it.
type reviewTarget struct {
	ref      string
	staged   bool
	id       string // overlay item ID, empty for a typed ref
	label    string // short display name, e.g. "PR #12" or "branch feature"; empty for typed refs and the original
	branch   string // review-session branch key when known (switcher branch, PR head); empty derives it from ref
	original bool   // true when returning to the review revdiff was started with
}

// refOrigin is the startup review, captured at construction so the switcher's
// "original" entry can restore every piece of mode-dependent state the
// composition root resolved for it.
type refOrigin struct {
	ref               string
	staged            bool
	showUntracked     bool
	commitsApplicable bool
	sourceEditor      SourceEditorPolicy
	reviewCfg         *ReviewInfoConfig
}

// refSwitchState holds the review-target switcher: the injected source, the
// startup review, lists loaded for the open overlay (each under listSeq), the
// in-flight resolution (resolveSeq), a pending annotation-drop confirmation,
// and a transient status-bar hint cleared on the next key press.
type refSwitchState struct {
	source         RefSource
	origin         refOrigin
	activeID       string // overlay ID of the review currently shown
	label          string // label of the switched-to review; empty while on the original or a typed ref
	branches       refsource.BranchList
	branchesErr    error
	branchesLoaded bool
	prs            []refsource.PullRequest
	prsErr         error
	prsLoaded      bool
	listSeq        uint64
	resolveSeq     uint64
	pending        *reviewTarget
	hint           string
}

type refBranchesLoadedMsg struct {
	seq  uint64
	list refsource.BranchList
	err  error
}

type refPullRequestsLoadedMsg struct {
	seq uint64
	prs []refsource.PullRequest
	err error
}

type refResolvedMsg struct {
	seq    uint64
	target reviewTarget
	err    error
}

// ReviewRef returns the ref and staged flag of the review currently shown,
// which differ from the startup values after a runtime switch. The composition
// root records them in the history auto-save.
func (m Model) ReviewRef() (ref string, staged bool) {
	return m.cfg.ref, m.cfg.staged
}

// openRefSwitcher opens the switcher and starts loading branches and pull
// requests in parallel. Previously loaded lists stay visible (marked loading)
// until the refreshed ones land.
func (m *Model) openRefSwitcher() tea.Cmd {
	if m.refs.source == nil {
		m.refs.hint = "Switching review is not available in this mode"
		return nil
	}
	m.refs.listSeq++
	m.refs.branchesLoaded = false
	m.refs.prsLoaded = false
	m.overlay.OpenRefPicker(m.buildRefPickerSpec())
	seq, src := m.refs.listSeq, m.refs.source
	return tea.Batch(
		func() tea.Msg {
			list, err := src.Branches()
			return refBranchesLoadedMsg{seq: seq, list: list, err: err}
		},
		func() tea.Msg {
			prs, err := src.PullRequests()
			return refPullRequestsLoadedMsg{seq: seq, prs: prs, err: err}
		},
	)
}

// handleRefSwitchMsg routes the switcher's async results.
func (m Model) handleRefSwitchMsg(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case refBranchesLoadedMsg:
		return m.handleRefBranchesLoaded(msg)
	case refPullRequestsLoadedMsg:
		return m.handleRefPullRequestsLoaded(msg)
	case refResolvedMsg:
		return m.handleRefResolved(msg)
	}
	return m, nil
}

func (m Model) handleRefBranchesLoaded(msg refBranchesLoadedMsg) (tea.Model, tea.Cmd) {
	if msg.seq != m.refs.listSeq {
		return m, nil
	}
	m.refs.branches, m.refs.branchesErr, m.refs.branchesLoaded = msg.list, msg.err, true
	m.overlay.UpdateRefPicker(m.buildRefPickerSpec())
	return m, nil
}

func (m Model) handleRefPullRequestsLoaded(msg refPullRequestsLoadedMsg) (tea.Model, tea.Cmd) {
	if msg.seq != m.refs.listSeq {
		return m, nil
	}
	m.refs.prs, m.refs.prsErr, m.refs.prsLoaded = msg.prs, msg.err, true
	m.overlay.UpdateRefPicker(m.buildRefPickerSpec())
	return m, nil
}

// buildRefPickerSpec assembles the switcher's sections from the loaded lists.
// All repository-supplied text (titles, branch names, subjects, error output)
// is sanitized and flattened to one line here; the overlay sanitizes again.
func (m Model) buildRefPickerSpec() overlay.RefPickerSpec {
	items := make([]overlay.RefItem, 0, 1+len(m.refs.prs)+len(m.refs.branches.Branches))
	items = append(items, overlay.RefItem{ID: refIDOriginal, Section: "original", Label: m.originLabel(), Detail: "the review revdiff started with"})
	for _, pr := range m.refs.prs {
		detail := m.oneLine(pr.Head) + " → " + m.oneLine(pr.Base)
		if pr.Author != "" {
			detail += " · @" + m.oneLine(pr.Author)
		}
		if pr.Draft {
			detail += " · draft"
		}
		items = append(items, overlay.RefItem{
			ID:      refIDPRPrefix + strconv.Itoa(pr.Number),
			Section: "pull requests",
			Label:   fmt.Sprintf("#%d %s", pr.Number, m.oneLine(pr.Title)),
			Detail:  detail,
		})
	}
	base := m.refs.branches.Base
	for _, b := range m.refs.branches.Branches {
		var parts []string
		if b.Current {
			parts = append(parts, "current")
		}
		switch {
		case b.Ref != "":
			parts = append(parts, "vs "+m.oneLine(base))
		case base == "":
			parts = append(parts, "no base branch found")
		default:
			parts = append(parts, "base branch")
		}
		if b.Age != "" {
			parts = append(parts, m.oneLine(b.Age))
		}
		if b.Subject != "" {
			parts = append(parts, m.oneLine(b.Subject))
		}
		items = append(items, overlay.RefItem{
			ID:      refIDBranchPrefix + b.Name,
			Section: "branches",
			Label:   m.oneLine(b.Name),
			Detail:  strings.Join(parts, " · "),
		})
	}

	var notices []string
	if m.refs.branchesErr != nil {
		notices = append(notices, "branches unavailable: "+m.oneLine(m.refs.branchesErr.Error()))
	}
	if m.refs.prsErr != nil {
		notices = append(notices, "pull requests unavailable: "+m.oneLine(m.refs.prsErr.Error()))
	}
	current := m.refs.label
	if current == "" {
		current = m.currentRefLabel()
	}
	return overlay.RefPickerSpec{
		Current:  current,
		ActiveID: m.refs.activeID,
		Items:    items,
		Loading:  !m.refs.branchesLoaded || !m.refs.prsLoaded,
		Notices:  notices,
	}
}

// originLabel describes the startup review in the switcher's "original" row.
func (m Model) originLabel() string {
	return m.refLabel(m.refs.origin.ref, m.refs.origin.staged)
}

func (m Model) currentRefLabel() string {
	return m.refLabel(m.cfg.ref, m.cfg.staged)
}

func (m Model) refLabel(ref string, staged bool) string {
	switch {
	case staged:
		return "staged changes"
	case ref == "":
		return "working tree changes"
	default:
		return m.oneLine(ref)
	}
}

// handleRefChoice turns an overlay selection into a review target. Branches
// and the original resolve immediately; pull requests (fetch) and typed refs
// (validation) resolve asynchronously and land in handleRefResolved.
func (m Model) handleRefChoice(c *overlay.RefChoice) (tea.Model, tea.Cmd) {
	if c == nil || m.refs.source == nil {
		return m, nil
	}
	m.refs.resolveSeq++ // supersede any in-flight resolution
	seq, src := m.refs.resolveSeq, m.refs.source
	switch {
	case c.Raw != "":
		raw := strings.TrimSpace(c.Raw)
		m.refs.hint = "Checking " + m.oneLine(raw) + "…"
		return m, func() tea.Msg {
			return refResolvedMsg{seq: seq, target: reviewTarget{ref: raw}, err: src.CheckRef(raw)}
		}
	case c.ID == refIDOriginal:
		o := m.refs.origin
		cmd := m.requestRefSwitch(reviewTarget{ref: o.ref, staged: o.staged, id: refIDOriginal, original: true})
		return m, cmd
	case strings.HasPrefix(c.ID, refIDBranchPrefix):
		name := strings.TrimPrefix(c.ID, refIDBranchPrefix)
		for _, b := range m.refs.branches.Branches {
			if b.Name != name {
				continue
			}
			if b.Ref == "" {
				m.refs.hint = fmt.Sprintf("%s is the base branch, nothing to compare — type a ref such as %s~5..%s instead", m.oneLine(name), m.oneLine(name), m.oneLine(name))
				return m, nil
			}
			cmd := m.requestRefSwitch(reviewTarget{ref: b.Ref, id: c.ID, label: "branch " + m.oneLine(name), branch: name})
			return m, cmd
		}
		return m, nil
	case strings.HasPrefix(c.ID, refIDPRPrefix):
		n, err := strconv.Atoi(strings.TrimPrefix(c.ID, refIDPRPrefix))
		if err != nil {
			return m, nil
		}
		id, label, head := c.ID, "PR #"+strconv.Itoa(n), m.pullRequestHead(n)
		m.refs.hint = "Fetching " + label + "…"
		return m, func() tea.Msg {
			ref, err := src.PullRequestRef(n)
			return refResolvedMsg{seq: seq, target: reviewTarget{ref: ref, id: id, label: label, branch: head}, err: err}
		}
	default:
		return m, nil
	}
}

// pullRequestHead returns the head branch name of pull request n from the loaded
// list, or "" when it is not listed. A pull request's review session belongs to
// its head branch, so it is shared with a local checkout of that branch.
func (m Model) pullRequestHead(n int) string {
	for _, pr := range m.refs.prs {
		if pr.Number == n {
			return pr.Head
		}
	}
	return ""
}

func (m Model) handleRefResolved(msg refResolvedMsg) (tea.Model, tea.Cmd) {
	if msg.seq != m.refs.resolveSeq {
		return m, nil
	}
	if msg.err != nil {
		m.refs.hint = "Switch failed: " + m.oneLine(msg.err.Error())
		return m, nil
	}
	cmd := m.requestRefSwitch(msg.target)
	return m, cmd
}

// requestRefSwitch applies t, first asking for confirmation when annotations
// would be dropped — they reference line numbers of the current diff. Mirrors
// R reload, including the --no-confirm-reload and --no-status-bar opt-outs
// (without a status bar the prompt could not be seen).
func (m *Model) requestRefSwitch(t reviewTarget) tea.Cmd {
	if t.ref == m.cfg.ref && t.staged == m.cfg.staged {
		m.refs.hint = "Already reviewing " + m.targetLabel(t)
		return nil
	}
	if m.store.Count() > 0 && !m.sessionsActive() && !m.cfg.noStatusBar && !m.cfg.noConfirmReload {
		m.refs.pending = &t
		m.refs.hint = fmt.Sprintf("Annotations will be dropped — press y to switch to %s, any other key to cancel", m.targetLabel(t))
		return nil
	}
	return m.switchRef(t)
}

func (m Model) handlePendingRefSwitch(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	t := *m.refs.pending
	m.refs.pending = nil
	if msg.String() != "y" {
		m.refs.hint = "Switch canceled"
		return m, nil
	}
	cmd := m.switchRef(t)
	return m, cmd
}

func (m Model) targetLabel(t reviewTarget) string {
	if t.label != "" {
		return t.label
	}
	return m.refLabel(t.ref, t.staged)
}

// switchRef makes t the reviewed diff and reloads everything derived from the
// ref through triggerReload (file list, commit log, review stats, reviewed-mark
// fingerprints). Annotations are cleared like on R reload. A switch away from
// the original is a ref review: staged is off, the commit log applies,
// untracked files are hidden for ranges (working-tree state is not part of a
// historical diff, as with two-ref startup), and source edits no longer reload
// the diff or guard annotated files (both are working-tree review rules).
// Returning to the original restores the composition root's verdicts for it.
func (m *Model) switchRef(t reviewTarget) tea.Cmd {
	m.applyReloadCleanup()
	m.refs.pending = nil
	m.cfg.ref = t.ref
	m.cfg.staged = t.staged
	o := m.refs.origin
	if t.original {
		m.modes.showUntracked = o.showUntracked
		m.commits.applicable = o.commitsApplicable
		m.cfg.sourceEditorPolicy = o.sourceEditor
		m.review.cfg = o.reviewCfg
	} else {
		if strings.Contains(t.ref, "..") {
			m.modes.showUntracked = false
		}
		m.commits.applicable = m.commits.source != nil
		m.cfg.sourceEditorPolicy = o.sourceEditor
		m.cfg.sourceEditorPolicy.ReloadAfterCleanExit = false
		m.cfg.sourceEditorPolicy.DisallowAnnotatedFileEditing = false
		if o.reviewCfg != nil {
			cp := *o.reviewCfg
			cp.Ref, cp.Staged = t.ref, false
			m.review.cfg = &cp
		}
	}
	m.commits.err = nil
	m.commits.truncated = false
	m.refs.activeID = t.id
	m.refs.label = t.label
	m.refs.hint = "Reviewing " + m.targetLabel(t)
	// every mutation has already been saved, so the current session is simply
	// left behind; the target's session is validated by the reload below.
	m.session.present = nil
	m.openSession(session.Request{Ref: t.ref, Staged: t.staged, Branch: t.branch})
	return m.triggerReload()
}

// oneLine sanitizes repository- or network-supplied text for single-line
// display: escape sequences and control bytes are stripped and any whitespace
// run (including newlines and tabs) collapses to one space.
func (m Model) oneLine(s string) string {
	return strings.Join(strings.Fields(diff.SanitizeCommitText(s)), " ")
}
