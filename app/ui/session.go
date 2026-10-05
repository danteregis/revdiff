package ui

//go:generate moq -out mocks/session_store.go -pkg mocks -skip-ensure -fmt goimports . SessionStore

import (
	"fmt"
	"log"
	"reflect"
	"slices"
	"sort"
	"strings"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/umputun/revdiff/app/annotation"
	"github.com/umputun/revdiff/app/diff"
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
	preloaded   bool   // --annotations seeded the store; the startup open keeps it instead of loading the session's
	confirmNew  bool   // waiting for y to confirm new_session
	hint        string // transient status-bar message; cleared on next key press
}

// openSession makes the session req selects the active one, seeds the file
// tree with its reviewed marks and replaces the annotation store with its
// annotations. The next file-list load validates both: marks through the
// regular fingerprint pipeline (loadReviewedFingerprints + ReconcileReviewed),
// where unchanged files stay reviewed and changed ones read as changed since
// review; annotations through re-anchoring (reanchorAnnotations), where those
// whose code changed become outdated. Annotations preloaded with --annotations
// replace the session's on the startup open instead and are saved into it.
// Failures are logged and leave sessions inactive for this review.
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
	preloaded := m.session.preloaded
	m.session.preloaded = false
	if preloaded {
		m.saveSession() // --annotations replaces the session's annotations
	} else {
		m.store.Clear()
		for _, a := range sess.Annotations {
			m.store.Add(a)
		}
	}
	m.invalidateRenderCaches()

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
	if n := m.store.Count(); n > 0 {
		parts = append(parts, m.plural(n, "annotation", "annotations"))
		if outdated := m.annotationCount(annotation.StatusOutdated); outdated > 0 {
			parts = append(parts, fmt.Sprintf("%d outdated", outdated))
		}
	}
	m.session.hint = strings.Join(parts, " · ")
}

// annotationCount counts the stored annotations in status.
func (m Model) annotationCount(status annotation.Status) int {
	n := 0
	for _, anns := range m.store.All() {
		for _, a := range anns {
			if a.Status == status {
				n++
			}
		}
	}
	return n
}

// plural formats n with the singular or plural noun.
func (m Model) plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

// setSessionPresent records the paths of an accepted file list.
func (m *Model) setSessionPresent(paths []string) {
	m.session.present = make(map[string]struct{}, len(paths))
	for _, p := range paths {
		m.session.present[p] = struct{}{}
	}
}

// saveSession merges the tree's reviewed state into the active session, takes
// the full annotation store as the session's annotations, and writes it. Only paths present in the current file list are touched: a
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
	m.session.cur.Annotations = m.sessionAnnotations()
	if err := m.session.store.Save(m.session.cur); err != nil {
		log.Printf("[WARN] save review session: %v", err)
		m.session.hint = "Saving the review session failed"
	}
}

// sessionAnnotations flattens the store into the session's annotation list,
// ordered by file then line.
func (m Model) sessionAnnotations() []annotation.Annotation {
	out := make([]annotation.Annotation, 0, m.store.Count())
	for _, file := range m.store.Files() {
		out = append(out, m.store.Get(file)...)
	}
	return out
}

// sessionsActive reports whether a review session is persisting this review.
func (m Model) sessionsActive() bool {
	return m.session.store != nil && m.session.cur != nil
}

// MarkDelivered records that the pending annotations were handed to the agent
// (the exit output was written) and saves the session, so the next run does
// not send them again. No-op without an active session: without persistence
// there is no next run to protect.
func (m Model) MarkDelivered() {
	if !m.sessionsActive() {
		return
	}
	if m.store.MarkDelivered() > 0 {
		m.saveSession()
	}
}

// discardPendingAnnotations drops the annotations Q discards — the ones not
// yet delivered — from the store and the session. Delivered, outdated and
// resolved annotations are review history and stay.
func (m *Model) discardPendingAnnotations() {
	if !m.sessionsActive() {
		return
	}
	if m.store.DiscardPending() > 0 {
		m.saveSession()
	}
}

// reanchorAnnotations re-anchors the captured annotations of every file that
// is still in the diff against its current full-context effective diff, using
// the same bounded worker pool as loadReviewedFingerprints. Runs inside the
// loadFiles command, so it only reads its snapshot; handleFilesLoaded applies
// the result. A file whose diff cannot be fetched is left out (and warned
// about), so its annotations keep their previous state.
func (m Model) reanchorAnnotations(entries []diff.FileEntry, before map[string][]annotation.Annotation) (map[string][]annotation.Annotation, []string) {
	if len(before) == 0 {
		return nil, nil
	}
	jobs := make([]diff.FileEntry, 0, len(before))
	for _, entry := range entries {
		if _, ok := before[entry.Path]; ok {
			jobs = append(jobs, entry)
		}
	}
	if len(jobs) == 0 {
		return nil, nil
	}

	type result struct {
		path string
		anns []annotation.Annotation
		err  error
	}
	jobCh := make(chan diff.FileEntry, len(jobs))
	resultCh := make(chan result, len(jobs))
	for _, entry := range jobs {
		jobCh <- entry
	}
	close(jobCh)
	for range min(maxReviewFingerprintWorkers, len(jobs)) {
		go func() {
			for entry := range jobCh {
				lines, err := m.fetchEffectiveFileDiff(entry, 0, true)
				res := result{path: entry.Path, err: err}
				if err == nil {
					res.anns, _ = annotation.ReanchorFile(before[entry.Path], lines)
				}
				resultCh <- res
			}
		}()
	}

	out := make(map[string][]annotation.Annotation, len(jobs))
	var warnings []string
	for range jobs {
		res := <-resultCh
		if res.err != nil {
			warnings = append(warnings, fmt.Sprintf("re-anchor annotations %s: %v", res.path, res.err))
			continue
		}
		out[res.path] = res.anns
	}
	sort.Strings(warnings)
	return out, warnings
}

// applyReanchored applies a file-list load's re-anchoring to the store. A file
// edited while the load ran is skipped (its annotations no longer match the
// snapshot; the next load re-validates them). Annotations of a file that left
// the diff become outdated — resolved ones stay resolved — and keep their
// lines. Any change is saved to the session.
func (m *Model) applyReanchored(msg filesLoadedMsg, entries []diff.FileEntry) {
	if !m.sessionsActive() || len(msg.annotationsBefore) == 0 {
		return
	}
	present := make(map[string]bool, len(entries))
	for _, e := range entries {
		present[e.Path] = true
	}
	changed := false
	for file, snapshot := range msg.annotationsBefore {
		current := m.store.Get(file)
		if !reflect.DeepEqual(current, snapshot) {
			continue
		}
		var next []annotation.Annotation
		if present[file] {
			re, ok := msg.reanchored[file]
			if !ok {
				continue
			}
			next = slices.Clone(re)
		} else {
			next = slices.Clone(snapshot)
			for i := range next {
				if next[i].Status != annotation.StatusResolved {
					next[i].Status = annotation.StatusOutdated
				}
			}
		}
		m.store.ReplaceFile(file, next)
		if reflect.DeepEqual(m.store.Get(file), current) {
			continue
		}
		changed = true
	}
	if !changed {
		return
	}
	m.invalidateRenderCaches()
	if m.tree.FilterActive() {
		m.tree.RefreshFilter(m.annotatedFiles())
	}
	m.saveSession()
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
