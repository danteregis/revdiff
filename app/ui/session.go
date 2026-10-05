package ui

//go:generate moq -out mocks/session_store.go -pkg mocks -skip-ensure -fmt goimports . SessionStore

import (
	"fmt"
	"log"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/umputun/revdiff/app/session"
)

// SessionStore persists review sessions per branch: Open loads (or starts) the
// session a request selects, Save writes one back. Implemented by
// *session.Store (git only). Wired through ModelConfig.Sessions; nil disables
// session persistence entirely.
type SessionStore interface {
	Open(req session.Request) (session.Opened, error)
	Save(s *session.Session) error
}

// sessionState holds the active review session. present is the set of paths in
// the last accepted file list: a save only updates or removes marks for those
// paths, so a narrowed run (--only, --include, untracked off) never shrinks
// the persisted set. note is the headline shown, with mark counts, once the
// next file list has validated the session's marks.
type sessionState struct {
	store       SessionStore
	cur         *session.Session
	present     map[string]struct{}
	note        string
	noteCounts  bool // append reviewed / changed counts to note (resumed sessions)
	notePending bool
	mismatch    bool   // the loaded marks were fingerprinted by another revdiff version
	confirmNew  bool   // waiting for y to confirm new_session
	hint        string // transient status-bar message; cleared on next key press
}

// openSession makes the session req selects the active one and seeds the file
// tree with its reviewed marks, which the next file-list load validates
// through the regular fingerprint pipeline (loadReviewedFingerprints +
// ReconcileReviewed): unchanged files stay reviewed, changed ones read as
// changed since review. Failures are logged and leave sessions inactive for
// this review.
func (m *Model) openSession(req session.Request) {
	if m.session.store == nil {
		return
	}
	opened, err := m.session.store.Open(req)
	if err != nil || opened.Session == nil {
		log.Printf("[WARN] review session unavailable: %v", err)
		m.session.cur = nil
		m.session.hint = "Review session unavailable"
		return
	}
	sess := opened.Session
	if sess.Reviewed == nil {
		sess.Reviewed = map[string]string{}
	}
	m.session.cur = sess
	m.session.mismatch = opened.FingerprintMismatch
	m.tree.ResetReviewed(sess.Reviewed)
	m.reviewed.cache = make(map[string]string)
	m.reviewed.pending = make(map[string]uint64)

	label := m.sessionLabel(sess)
	m.session.noteCounts = opened.Resumed
	switch {
	case opened.Resumed:
		m.session.note = "Resumed session " + label
	case req.Fresh:
		m.session.note = "Started a new session " + label + " (previous sessions are kept)"
	default:
		m.session.note = ""
	}
	m.session.notePending = true
}

// sessionLabel names a session for status messages: its name when it has one,
// plus the branch it belongs to.
func (m Model) sessionLabel(s *session.Session) string {
	label := "for " + m.oneLine(s.Branch)
	if s.Name != "" {
		label = "“" + m.oneLine(s.Name) + "” " + label
	}
	return label
}

// finishSessionNote turns the pending open note into a status-bar hint once
// the file list that validates the session's marks has been applied.
func (m *Model) finishSessionNote() {
	if !m.session.notePending {
		return
	}
	m.session.notePending = false
	if m.session.note == "" {
		return
	}
	parts := []string{m.session.note}
	if !m.session.noteCounts {
		m.session.hint = m.session.note
		return
	}
	reviewed, changed := m.tree.ReviewedCount(), m.tree.ChangedSinceReviewCount()
	if m.session.mismatch && changed > 0 {
		parts = append(parts, fmt.Sprintf("%d reviewed marks from another revdiff version could not be verified", changed))
	} else {
		parts = append(parts, fmt.Sprintf("%d reviewed", reviewed))
		if changed > 0 {
			parts = append(parts, fmt.Sprintf("%d changed since review", changed))
		}
	}
	m.session.hint = strings.Join(parts, " · ")
}

// setSessionPresent records the paths of an accepted file list.
func (m *Model) setSessionPresent(paths []string) {
	m.session.present = make(map[string]struct{}, len(paths))
	for _, p := range paths {
		m.session.present[p] = struct{}{}
	}
}

// saveSession merges the tree's reviewed state into the active session and
// writes it. Only paths present in the current file list are touched: a
// reviewed path stores its fingerprint, a path changed since review keeps the
// fingerprint the reviewer approved (so it stays "changed" next time), and any
// other present path loses its mark. A failed write is logged and surfaced as
// a hint; the review continues.
func (m *Model) saveSession() {
	if m.session.store == nil || m.session.cur == nil {
		return
	}
	marks := m.tree.ReviewedFingerprints()
	for path := range m.session.present {
		if fp, ok := marks[path]; ok {
			m.session.cur.Reviewed[path] = fp
			continue
		}
		if m.tree.IsChangedSinceReview(path) {
			continue
		}
		delete(m.session.cur.Reviewed, path)
	}
	if err := m.session.store.Save(m.session.cur); err != nil {
		log.Printf("[WARN] save review session: %v", err)
		m.session.hint = "Saving the review session failed"
	}
}

// handleNewSession starts a fresh session for the current branch, asking for
// confirmation first when the current review has any state to leave behind.
// The previous session stays on disk.
func (m Model) handleNewSession() (tea.Model, tea.Cmd) {
	if m.session.store == nil || m.session.cur == nil {
		m.session.hint = "Review sessions are not available in this mode"
		return m, nil
	}
	hasState := m.tree.ReviewedCount() > 0 || m.tree.ChangedSinceReviewCount() > 0 || m.store.Count() > 0
	if hasState && !m.cfg.noStatusBar {
		m.session.confirmNew = true
		m.session.hint = "Start a new session? Marks and annotations are set aside (the current session is kept) — press y to confirm, any other key to cancel"
		return m, nil
	}
	m.startNewSession()
	return m, nil
}

func (m Model) handlePendingNewSession(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	m.session.confirmNew = false
	if msg.String() != "y" {
		m.session.hint = "New session canceled"
		return m, nil
	}
	m.startNewSession()
	return m, nil
}

// startNewSession clears the review state shown in the UI and opens a fresh
// session on the same branch and diff. The file list does not change, so the
// present set is kept and nothing is reloaded.
func (m *Model) startNewSession() {
	branch := m.session.cur.Branch
	m.applyReloadCleanup()
	m.openSession(session.Request{Ref: m.cfg.ref, Staged: m.cfg.staged, Branch: branch, Fresh: true})
	m.finishSessionNote()
	if m.tree.UnreviewedFilterActive() {
		m.tree.RefreshUnreviewedFilter()
	}
	if m.file.name != "" {
		m.layout.viewport.SetContent(m.renderDiff())
	}
}
