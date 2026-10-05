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
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/umputun/revdiff/app/annotation"
	"github.com/umputun/revdiff/app/diff"
	"github.com/umputun/revdiff/app/fsutil"
)

// schemaVersion is the session file format version written by this revdiff.
const schemaVersion = 1

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

// Open returns the session req selects: a fresh one when req.Fresh, the named
// one (created when missing) when req.Name is set, and otherwise the most
// recently updated session of the branch, or a new one when the branch has
// none. A new session is not written until it has state (see Save).
func (s *Store) Open(req Request) (Opened, error) {
	branch := strings.TrimSpace(req.Branch)
	if branch == "" {
		branch = s.git.branchKey(req.Ref)
	}
	head := s.git.tipCommit(req.Ref)

	if found := s.existing(branch, req); found != nil {
		found.Ref = req.Ref
		if head != "" {
			found.Head = head
		}
		mismatch := len(found.Reviewed) > 0 && found.FingerprintVersion != diff.FileFingerprintVersion
		return Opened{Session: found, Resumed: true, FingerprintMismatch: mismatch}, nil
	}
	return Opened{Session: s.newSession(branch, req.Name, req.Ref, head)}, nil
}

// existing returns the stored session req resumes on branch, or nil when it
// asks for a fresh one or none matches.
func (s *Store) existing(branch string, req Request) *Session {
	if req.Fresh {
		return nil
	}
	sessions := s.list(branch)
	if req.Name != "" {
		return s.byName(sessions, req.Name)
	}
	if len(sessions) == 0 {
		return nil
	}
	return sessions[0]
}

// Save writes sess atomically, stamping its version fields and update time. A
// session that has no state and was never written is skipped, so merely
// opening revdiff leaves nothing behind; once written, a session is rewritten
// even when its state becomes empty, so removed marks cannot resurrect.
func (s *Store) Save(sess *Session) error {
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
	sess.Updated = s.now().UTC()
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

// escapeBranch maps a branch key to a single safe directory name: every byte
// outside [A-Za-z0-9._-] (including "/") is percent-encoded, and names that
// would be special path elements get a prefix.
func escapeBranch(branch string) string {
	var b strings.Builder
	for i := range len(branch) {
		c := branch[i]
		if isSafeByte(c) {
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
