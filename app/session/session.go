// Package session persists revdiff review sessions: which files were marked
// reviewed (and at which semantic diff fingerprint) and the annotations written
// during the review, with their status, delivery state and anchors. Sessions are attached to a git branch
// so a review can be resumed after the code under review changes.
//
// Layout on disk:
//
//	<root>/<repo dir>/<branch dir>/<session id>.json
//
// <repo dir> is a readable repository name plus a hash of the repository
// identity (the normalized origin URL, else the absolute git common dir, so all
// worktrees of one clone share sessions); it is never the bare basename, which
// collides across unrelated checkouts. <branch dir> is the path-escaped branch
// key. Every write is atomic (temp file + rename, mode 0600).
//
// The package owns every git process the feature runs, so app/ui stays free of
// exec concerns and consumes it through the consumer-side ui.SessionStore
// interface.
package session

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"maps"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/umputun/revdiff/app/annotation"
	"github.com/umputun/revdiff/app/diff"
	"github.com/umputun/revdiff/app/fsutil"
)

const (
	// schemaVersion is the session file format version written by this revdiff.
	schemaVersion = 1
	// maxInheritCandidates caps how many other-branch sessions are checked for
	// ancestry when a branch has no session yet; each costs a git call.
	maxInheritCandidates = 20
)

// Session is one persisted review of a branch.
type Session struct {
	Version            int       `json:"version"`
	FingerprintVersion string    `json:"fingerprint_version"`
	ID                 string    `json:"id"`
	Name               string    `json:"name,omitempty"`
	Created            time.Time `json:"created"`
	Updated            time.Time `json:"updated"`
	RepoID             string    `json:"repo_id"`
	Branch             string    `json:"branch"`
	Ref                string    `json:"ref,omitempty"`  // reviewed ref when last saved; empty for working-tree review
	Head               string    `json:"head,omitempty"` // commit at the tip of the reviewed ref when last saved
	// InheritedFrom names the branch whose session this one was copied from
	// when the branch had none of its own; empty otherwise.
	InheritedFrom string `json:"inherited_from,omitempty"`
	// Reviewed maps a path to the semantic diff fingerprint (diff.FileFingerprint)
	// of the content the reviewer approved. A path whose current fingerprint
	// differs is "changed since review".
	Reviewed map[string]string `json:"reviewed,omitempty"`
	// Annotations are every annotation of the review, including outdated,
	// resolved and delivered ones, each with the anchor used to find it again.
	Annotations []annotation.Annotation `json:"annotations,omitempty"`
}

// empty reports whether the session carries no review state worth persisting.
func (s *Session) empty() bool {
	return len(s.Reviewed) == 0 && len(s.Annotations) == 0
}

// Request selects the session Open returns.
type Request struct {
	Ref    string // reviewed ref, empty for working-tree review
	Staged bool   // staged review (resolves to the current branch like working-tree review)
	Branch string // branch key override, e.g. a pull request's head branch; empty derives it from Ref
	Name   string // resume the session with this name, or create it
	ID     string // resume exactly this session of the branch (error when it does not exist)
	Fresh  bool   // start a new session even when one exists for the branch
}

// Opened is the result of Open.
type Opened struct {
	Session *Session
	// Resumed is true when an existing session was loaded from disk.
	Resumed bool
	// FingerprintMismatch is true when the loaded session's reviewed marks were
	// computed by a different fingerprint version, so they cannot be verified
	// and every one of them will read as changed since review.
	FingerprintMismatch bool
	// InheritedFrom names the branch whose session was copied because this
	// branch had none; the copy is already saved. Empty otherwise.
	InheritedFrom string
}

// Summary describes one stored session for the sessions picker.
type Summary struct {
	ID            string
	Name          string
	Updated       time.Time
	InheritedFrom string
	Reviewed      int // reviewed marks
	Open          int // open annotations (pending or delivered)
	Outdated      int // outdated annotations
	Resolved      int // resolved annotations
}

// Store reads and writes the sessions of one repository.
type Store struct {
	dir     string // <root>/<repo dir>
	repoID  string
	git     gitRunner
	now     func() time.Time
	newID   func(time.Time) string
	warnLog func(format string, args ...any)
}

// New returns a Store for the git repository at repoDir, keeping its sessions
// under root (typically ~/.config/revdiff/sessions). It resolves the repository
// identity with git and fails when that is impossible.
func New(root, repoDir string) (*Store, error) {
	if root == "" {
		return nil, errors.New("sessions directory is not set")
	}
	g := gitRunner{dir: repoDir}
	id, name, err := g.repoIdentity()
	if err != nil {
		return nil, err
	}
	return &Store{
		dir:     filepath.Join(root, sanitizeName(name)+"-"+id[:12]),
		repoID:  id,
		git:     g,
		now:     time.Now,
		newID:   newSessionID,
		warnLog: log.Printf,
	}, nil
}

// DefaultRoot returns ~/.config/revdiff/sessions, or "" when the home directory
// is unknown.
func DefaultRoot() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".config", "revdiff", "sessions")
}

// Open returns the session req selects: a fresh one when req.Fresh, exactly
// req.ID when set, the named one (created when missing) when req.Name is set,
// and otherwise the most recently updated session of the branch. A branch with
// no session at all inherits one: the nearest session of another branch whose
// saved head is an ancestor of this branch's tip is copied into a new session
// for this branch (see inherit). Failing that a new session is returned, which
// is not written until it has state (see Save).
func (s *Store) Open(req Request) (Opened, error) {
	branch := strings.TrimSpace(req.Branch)
	if branch == "" {
		branch = s.git.branchKey(req.Ref)
	}
	head := s.git.tipCommit(req.Ref)

	sessions := s.list(branch)
	if req.ID != "" && !req.Fresh && s.byID(sessions, req.ID) == nil {
		return Opened{}, fmt.Errorf("session %q not found", req.ID)
	}
	if found := s.existing(sessions, req); found != nil {
		found.Ref = req.Ref
		if head != "" {
			found.Head = head
		}
		mismatch := len(found.Reviewed) > 0 && found.FingerprintVersion != diff.FileFingerprintVersion
		return Opened{Session: found, Resumed: true, FingerprintMismatch: mismatch}, nil
	}
	if !req.Fresh && req.Name == "" && len(sessions) == 0 && head != "" {
		if inherited := s.inherit(branch, req.Ref, head); inherited != nil {
			mismatch := len(inherited.Reviewed) > 0 && inherited.FingerprintVersion != diff.FileFingerprintVersion
			return Opened{Session: inherited, Resumed: true, FingerprintMismatch: mismatch, InheritedFrom: inherited.InheritedFrom}, nil
		}
	}
	return Opened{Session: s.newSession(branch, req.Name, req.Ref, head)}, nil
}

// existing returns the stored session req resumes among sessions (one branch,
// newest first), or nil when it asks for a fresh one or none matches.
func (s *Store) existing(sessions []*Session, req Request) *Session {
	switch {
	case req.Fresh:
		return nil
	case req.ID != "":
		return s.byID(sessions, req.ID)
	case req.Name != "":
		return s.byName(sessions, req.Name)
	case len(sessions) == 0:
		return nil
	default:
		return sessions[0]
	}
}

func (s *Store) byID(sessions []*Session, id string) *Session {
	for _, sess := range sessions {
		if sess.ID == id {
			return sess
		}
	}
	return nil
}

// inherit copies the session of the nearest ancestor branch into a new session
// for branch, so a branch cut from a reviewed one starts with its review. It
// considers the most recently updated session of every other branch (at most
// maxInheritCandidates of them, newest first), keeps those whose saved head is
// an ancestor of tip, and picks the one with the fewest commits between its
// head and tip (the most recently updated on a tie). The parent is left
// untouched; the copy is saved immediately. The repository's base branches
// (main, master, origin's HEAD) never inherit: a branch merged into them would
// otherwise hand its review to the base. Returns nil when nothing qualifies.
func (s *Store) inherit(branch, ref, tip string) *Session {
	if s.git.isBaseBranch(branch) {
		return nil
	}
	dirs, err := os.ReadDir(s.dir)
	if err != nil {
		return nil
	}
	own := escapeBranch(branch)
	var candidates []*Session
	for _, d := range dirs {
		if !d.IsDir() || d.Name() == own {
			continue
		}
		key, uerr := unescapeBranch(d.Name())
		if uerr != nil {
			continue
		}
		if sessions := s.list(key); len(sessions) > 0 && sessions[0].Head != "" && !sessions[0].empty() {
			candidates = append(candidates, sessions[0])
		}
	}
	sort.SliceStable(candidates, func(i, j int) bool { return candidates[i].Updated.After(candidates[j].Updated) })
	if len(candidates) > maxInheritCandidates {
		candidates = candidates[:maxInheritCandidates]
	}

	var parent *Session
	best := -1
	for _, c := range candidates {
		dist, ok := s.git.ancestorDistance(c.Head, tip)
		if !ok || (best >= 0 && dist >= best) {
			continue
		}
		parent, best = c, dist
	}
	if parent == nil {
		return nil
	}

	child := s.newSession(branch, "", ref, tip)
	child.InheritedFrom = parent.Branch
	child.Reviewed = maps.Clone(parent.Reviewed)
	if child.Reviewed == nil {
		child.Reviewed = map[string]string{}
	}
	child.Annotations = append([]annotation.Annotation(nil), parent.Annotations...)
	child.FingerprintVersion = parent.FingerprintVersion
	if err := s.Save(child); err != nil {
		s.warnLog("[WARN] sessions: save session inherited from %s: %v", parent.Branch, err)
	}
	return child
}

// List returns summaries of every stored session of branch, most recently
// updated first.
func (s *Store) List(branch string) ([]Summary, error) {
	sessions := s.list(branch)
	out := make([]Summary, 0, len(sessions))
	for _, sess := range sessions {
		sum := Summary{ID: sess.ID, Name: sess.Name, Updated: sess.Updated, InheritedFrom: sess.InheritedFrom, Reviewed: len(sess.Reviewed)}
		for _, a := range sess.Annotations {
			switch a.Status {
			case annotation.StatusOutdated:
				sum.Outdated++
			case annotation.StatusResolved:
				sum.Resolved++
			default:
				sum.Open++
			}
		}
		out = append(out, sum)
	}
	return out, nil
}

// Rename sets the name of session id of branch.
func (s *Store) Rename(branch, id, name string) error {
	sess, err := s.read(s.path(branch, id))
	if err != nil {
		return fmt.Errorf("rename session: %w", err)
	}
	sess.Branch = branch
	sess.Name = strings.TrimSpace(name)
	// keep the update time: renaming must not make a session the one resumed next
	if err := s.write(sess, false); err != nil {
		return fmt.Errorf("rename session: %w", err)
	}
	return nil
}

// Delete removes session id of branch from disk.
func (s *Store) Delete(branch, id string) error {
	if err := os.Remove(s.path(branch, id)); err != nil {
		return fmt.Errorf("delete session: %w", err)
	}
	return nil
}

// Save writes sess atomically, stamping its version fields and update time. A
// session that has no state and was never written is skipped, so merely
// opening revdiff leaves nothing behind; once written, a session is rewritten
// even when its state becomes empty, so removed marks cannot resurrect.
func (s *Store) Save(sess *Session) error {
	return s.write(sess, true)
}

// write saves sess; stamp sets its update time to now (a review change) while
// a metadata-only write such as a rename keeps it.
func (s *Store) write(sess *Session, stamp bool) error {
	if sess == nil || sess.ID == "" || sess.Branch == "" {
		return errors.New("save session: incomplete session")
	}
	path := s.path(sess.Branch, sess.ID)
	if sess.empty() {
		if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
			return nil
		}
	}
	if sess.Head == "" {
		sess.Head = s.git.tipCommit(sess.Ref)
	}
	sess.Version = schemaVersion
	sess.FingerprintVersion = diff.FileFingerprintVersion
	sess.RepoID = s.repoID
	if stamp || sess.Updated.IsZero() {
		sess.Updated = s.now().UTC()
	}
	data, err := json.MarshalIndent(sess, "", "  ")
	if err != nil {
		return fmt.Errorf("encode session: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return fmt.Errorf("create session directory: %w", err)
	}
	if err := fsutil.AtomicWriteFile(path, append(data, '\n')); err != nil {
		return fmt.Errorf("write session: %w", err)
	}
	return nil
}

// newSession builds an unsaved session for branch.
func (s *Store) newSession(branch, name, ref, head string) *Session {
	now := s.now().UTC()
	return &Session{
		Version:            schemaVersion,
		FingerprintVersion: diff.FileFingerprintVersion,
		ID:                 s.newID(now),
		Name:               name,
		Created:            now,
		Updated:            now,
		RepoID:             s.repoID,
		Branch:             branch,
		Ref:                ref,
		Head:               head,
		Reviewed:           map[string]string{},
	}
}

// list returns the readable sessions of branch, most recently updated first.
// Unreadable or malformed files are skipped with a warning.
func (s *Store) list(branch string) []*Session {
	dir := s.branchDir(branch)
	entries, err := os.ReadDir(dir)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			s.warnLog("[WARN] sessions: read %s: %v", dir, err)
		}
		return nil
	}
	var out []*Session
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".json" {
			continue
		}
		sess, err := s.read(filepath.Join(dir, e.Name()))
		if err != nil {
			s.warnLog("[WARN] sessions: %v", err)
			continue
		}
		sess.Branch = branch
		out = append(out, sess)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].Updated.Equal(out[j].Updated) {
			return out[i].Updated.After(out[j].Updated)
		}
		return out[i].ID > out[j].ID
	})
	return out
}

// read decodes one session file. The branch and id come from the file's
// location, not its contents, so a copied or hand-edited file cannot write
// itself into another branch's directory on the next save.
func (s *Store) read(path string) (*Session, error) {
	data, err := os.ReadFile(path) //nolint:gosec // path is built from the sessions directory listing
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var sess Session
	if err := json.Unmarshal(data, &sess); err != nil {
		return nil, fmt.Errorf("decode %s: %w", path, err)
	}
	if sess.Version > schemaVersion {
		return nil, fmt.Errorf("%s: written by a newer revdiff (schema %d)", path, sess.Version)
	}
	sess.ID = strings.TrimSuffix(filepath.Base(path), ".json")
	if sess.Reviewed == nil {
		sess.Reviewed = map[string]string{}
	}
	for i := range sess.Annotations {
		if sess.Annotations[i].File == "" {
			return nil, fmt.Errorf("%s: annotation %d has no file", path, i)
		}
	}
	return &sess, nil
}

func (s *Store) byName(sessions []*Session, name string) *Session {
	for _, sess := range sessions {
		if sess.Name == name {
			return sess
		}
	}
	return nil
}

func (s *Store) branchDir(branch string) string {
	return filepath.Join(s.dir, escapeBranch(branch))
}

func (s *Store) path(branch, id string) string {
	return filepath.Join(s.branchDir(branch), sanitizeName(id)+".json")
}

// newSessionID returns a sortable, collision-resistant session id.
func newSessionID(now time.Time) string {
	var b [3]byte
	_, _ = rand.Read(b[:])
	return now.UTC().Format("20060102T150405") + "-" + hex.EncodeToString(b[:])
}

// unescapeBranch is the inverse of escapeBranch.
func unescapeBranch(name string) (string, error) {
	name = strings.TrimPrefix(name, "_")
	var b strings.Builder
	for i := 0; i < len(name); i++ {
		if name[i] != '%' {
			b.WriteByte(name[i])
			continue
		}
		if i+2 >= len(name) {
			return "", fmt.Errorf("bad escape in %q", name)
		}
		v, err := hex.DecodeString(name[i+1 : i+3])
		if err != nil {
			return "", fmt.Errorf("bad escape in %q: %w", name, err)
		}
		b.Write(v)
		i += 2
	}
	return b.String(), nil
}

// escapeBranch maps a branch key to a single safe directory name: every byte
// outside [A-Za-z0-9._-] (including "/") is percent-encoded, and names that
// would be special path elements get a "_" prefix. A leading "_" of the key
// itself is encoded, so a leading "_" in the result is always that prefix and
// unescapeBranch can strip it.
func escapeBranch(branch string) string {
	var b strings.Builder
	for i := range len(branch) {
		c := branch[i]
		if isSafeByte(c) && (i > 0 || c != '_') {
			b.WriteByte(c)
			continue
		}
		fmt.Fprintf(&b, "%%%02X", c)
	}
	out := b.String()
	if out == "" || out == "." || out == ".." {
		out = "_" + out
	}
	return out
}

// sanitizeName keeps [A-Za-z0-9._-] and replaces everything else with "_".
func sanitizeName(s string) string {
	var b strings.Builder
	for i := range len(s) {
		if c := s[i]; isSafeByte(c) {
			b.WriteByte(c)
			continue
		}
		b.WriteByte('_')
	}
	out := strings.Trim(b.String(), ".")
	if out == "" {
		return "repo"
	}
	return out
}

func isSafeByte(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '.' || c == '_' || c == '-'
}
