package session

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/umputun/revdiff/app/annotation"
	"github.com/umputun/revdiff/app/diff"
)

// gitIn runs git in dir with a fixed identity and returns trimmed stdout.
func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...) //nolint:gosec // test helper, args constructed internally
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@test.com",
		"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@test.com", "GIT_CONFIG_NOSYSTEM=1")
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %v: %s", args, out)
	return strings.TrimSpace(string(out))
}

func commitFile(t *testing.T, dir, name, content string) {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600))
	gitIn(t, dir, "add", name)
	gitIn(t, dir, "commit", "-q", "-m", "change "+name)
}

// newRepo creates a repo on main with one commit and a feature branch with one
// more, leaving feature checked out.
func newRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	gitIn(t, dir, "-c", "init.defaultBranch=main", "init", "-q")
	commitFile(t, dir, "a.txt", "one\n")
	gitIn(t, dir, "checkout", "-q", "-b", "feature")
	commitFile(t, dir, "b.txt", "two\n")
	return dir
}

func newTestStore(t *testing.T, repo string) *Store {
	t.Helper()
	s, err := New(t.TempDir(), repo)
	require.NoError(t, err)
	clock := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s.now = func() time.Time { clock = clock.Add(time.Second); return clock }
	return s
}

func TestNew(t *testing.T) {
	t.Run("requires a root", func(t *testing.T) {
		_, err := New("", newRepo(t))
		require.Error(t, err)
	})
	t.Run("fails outside a repository", func(t *testing.T) {
		_, err := New(t.TempDir(), t.TempDir())
		require.Error(t, err)
	})
	t.Run("origin url identity is shared across clones and protocols", func(t *testing.T) {
		a, b := newRepo(t), newRepo(t)
		gitIn(t, a, "remote", "add", "origin", "https://GitHub.com/owner/proj.git")
		gitIn(t, b, "remote", "add", "origin", "git@github.com:owner/proj")
		root := t.TempDir()
		sa, err := New(root, a)
		require.NoError(t, err)
		sb, err := New(root, b)
		require.NoError(t, err)
		assert.Equal(t, sa.repoID, sb.repoID)
		assert.Equal(t, sa.dir, sb.dir)
		assert.True(t, strings.HasPrefix(filepath.Base(sa.dir), "proj-"), sa.dir)
	})
	t.Run("without a remote, worktrees share the common dir identity", func(t *testing.T) {
		repo := newRepo(t)
		wt := filepath.Join(t.TempDir(), "wt")
		gitIn(t, repo, "worktree", "add", "-q", wt, "main")
		root := t.TempDir()
		s1, err := New(root, repo)
		require.NoError(t, err)
		s2, err := New(root, wt)
		require.NoError(t, err)
		assert.Equal(t, s1.repoID, s2.repoID)
	})
	t.Run("same basename in different places does not collide", func(t *testing.T) {
		p1 := filepath.Join(t.TempDir(), "same")
		p2 := filepath.Join(t.TempDir(), "same")
		for _, p := range []string{p1, p2} {
			require.NoError(t, os.MkdirAll(p, 0o700))
			gitIn(t, p, "-c", "init.defaultBranch=main", "init", "-q")
			commitFile(t, p, "a.txt", "x\n")
		}
		root := t.TempDir()
		s1, err := New(root, p1)
		require.NoError(t, err)
		s2, err := New(root, p2)
		require.NoError(t, err)
		assert.NotEqual(t, s1.dir, s2.dir)
	})
}

func TestNormalizeRemoteURL(t *testing.T) {
	tests := []struct{ in, want string }{
		{"https://github.com/owner/repo.git", "github.com/owner/repo"},
		{"https://user:tok@GitHub.com:443/owner/repo/", "github.com/owner/repo"},
		{"ssh://git@github.com:22/owner/repo.git", "github.com/owner/repo"},
		{"git@github.com:owner/repo.git", "github.com/owner/repo"},
		{"/srv/git/repo.git", "/srv/git/repo.git"},
		{"file:///srv/git/repo.git", "/srv/git/repo.git"},
		{"", ""},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, normalizeRemoteURL(tt.in), tt.in)
	}
}

func TestGitRunner_BranchKey(t *testing.T) {
	repo := newRepo(t)
	g := gitRunner{dir: repo}
	head := gitIn(t, repo, "rev-parse", "HEAD")
	upstream := t.TempDir()
	gitIn(t, upstream, "-c", "init.defaultBranch=main", "init", "-q", "--bare")
	gitIn(t, repo, "remote", "add", "origin", upstream)
	gitIn(t, repo, "push", "-q", "origin", "feature:remote-only")
	gitIn(t, repo, "fetch", "-q", "origin")

	tests := []struct{ ref, want string }{
		{"", "feature"},
		{"main", "feature"},
		{"HEAD~1", "feature"},
		{"main..feature", "feature"},
		{"main...feature", "feature"},
		{"main..", "feature"},
		{"main..HEAD", "feature"},
		{"feature..main", "main"},
		{"main...origin/remote-only", "remote-only"},
		{"main.." + head, head},
		{"main...v1-tag-missing", "v1-tag-missing"},
	}
	for _, tt := range tests {
		assert.Equal(t, tt.want, g.branchKey(tt.ref), "ref %q", tt.ref)
	}

	t.Run("detached head", func(t *testing.T) {
		gitIn(t, repo, "checkout", "-q", "--detach", "HEAD")
		short := gitIn(t, repo, "rev-parse", "--short", "HEAD")
		assert.Equal(t, "detached-"+short, g.branchKey(""))
	})
}

func TestStore_BranchKeyAndDir(t *testing.T) {
	repo := newRepo(t)
	s := newTestStore(t, repo)
	assert.Equal(t, "feature", s.BranchKey(""))
	assert.Equal(t, "main", s.BranchKey("feature...main"))
	dir := s.BranchDir("feat/x")
	assert.Equal(t, s.dir, filepath.Dir(dir))
	assert.Equal(t, "feat%2Fx", filepath.Base(dir))

	opened, err := s.Open(Request{Ref: "main...feature"})
	require.NoError(t, err)
	assert.Equal(t, s.BranchDir(opened.Session.Branch), s.BranchDir(s.BranchKey("main...feature")),
		"BranchKey resolves the branch Open uses")
}

func TestGitRunner_TipCommit(t *testing.T) {
	repo := newRepo(t)
	g := gitRunner{dir: repo}
	head := gitIn(t, repo, "rev-parse", "HEAD")
	main := gitIn(t, repo, "rev-parse", "main")
	assert.Equal(t, head, g.tipCommit(""))
	assert.Equal(t, head, g.tipCommit("main.."))
	assert.Equal(t, main, g.tipCommit("feature..main"))
	assert.Empty(t, g.tipCommit("main..nope"))
	assert.Empty(t, g.tipCommit("main..-x"))
}

func TestStore_OpenSave(t *testing.T) {
	t.Run("new session is not written until it has state", func(t *testing.T) {
		repo := newRepo(t)
		s := newTestStore(t, repo)
		opened, err := s.Open(Request{})
		require.NoError(t, err)
		assert.False(t, opened.Resumed)
		assert.Equal(t, "feature", opened.Session.Branch)
		require.NoError(t, s.Save(opened.Session))
		_, statErr := os.Stat(s.path("feature", opened.Session.ID))
		assert.True(t, os.IsNotExist(statErr))

		opened.Session.Reviewed["b.txt"] = "fp"
		require.NoError(t, s.Save(opened.Session))
		info, err := os.Stat(s.path("feature", opened.Session.ID))
		require.NoError(t, err)
		assert.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	})

	t.Run("resume loads the latest session of the branch", func(t *testing.T) {
		repo := newRepo(t)
		s := newTestStore(t, repo)
		first, err := s.Open(Request{})
		require.NoError(t, err)
		first.Session.Reviewed["a"] = "1"
		require.NoError(t, s.Save(first.Session))
		second, err := s.Open(Request{Fresh: true})
		require.NoError(t, err)
		assert.NotEqual(t, first.Session.ID, second.Session.ID)
		second.Session.Reviewed["b"] = "2"
		require.NoError(t, s.Save(second.Session))

		got, err := s.Open(Request{})
		require.NoError(t, err)
		assert.True(t, got.Resumed)
		assert.False(t, got.FingerprintMismatch)
		assert.Equal(t, second.Session.ID, got.Session.ID)
		assert.Equal(t, map[string]string{"b": "2"}, got.Session.Reviewed)
		assert.Equal(t, gitIn(t, repo, "rev-parse", "HEAD"), got.Session.Head)

		// saving the older one again makes it the latest
		first.Session.Reviewed["c"] = "3"
		require.NoError(t, s.Save(first.Session))
		got, err = s.Open(Request{})
		require.NoError(t, err)
		assert.Equal(t, first.Session.ID, got.Session.ID)
	})

	t.Run("named sessions resume or are created", func(t *testing.T) {
		repo := newRepo(t)
		s := newTestStore(t, repo)
		named, err := s.Open(Request{Name: "deep-dive"})
		require.NoError(t, err)
		assert.False(t, named.Resumed)
		assert.Equal(t, "deep-dive", named.Session.Name)
		named.Session.Reviewed["x"] = "1"
		require.NoError(t, s.Save(named.Session))

		other, err := s.Open(Request{Fresh: true})
		require.NoError(t, err)
		other.Session.Reviewed["y"] = "1"
		require.NoError(t, s.Save(other.Session))

		got, err := s.Open(Request{Name: "deep-dive"})
		require.NoError(t, err)
		assert.True(t, got.Resumed)
		assert.Equal(t, named.Session.ID, got.Session.ID)
	})

	t.Run("branch override and ranges pick the branch directory", func(t *testing.T) {
		repo := newRepo(t)
		s := newTestStore(t, repo)
		got, err := s.Open(Request{Ref: "main...feature", Branch: "pr-head"})
		require.NoError(t, err)
		assert.Equal(t, "pr-head", got.Session.Branch)
		got, err = s.Open(Request{Ref: "feature...main"})
		require.NoError(t, err)
		assert.Equal(t, "main", got.Session.Branch)
		assert.Equal(t, "feature...main", got.Session.Ref)
	})

	t.Run("fingerprint version mismatch is reported", func(t *testing.T) {
		repo := newRepo(t)
		s := newTestStore(t, repo)
		opened, err := s.Open(Request{})
		require.NoError(t, err)
		opened.Session.Reviewed["a"] = "1"
		require.NoError(t, s.Save(opened.Session))

		path := s.path("feature", opened.Session.ID)
		data, err := os.ReadFile(path) //nolint:gosec // test path
		require.NoError(t, err)
		var raw map[string]any
		require.NoError(t, json.Unmarshal(data, &raw))
		raw["fingerprint_version"] = "revdiff-file-fingerprint-v0"
		data, err = json.Marshal(raw)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(path, data, 0o600))

		got, err := s.Open(Request{})
		require.NoError(t, err)
		assert.True(t, got.FingerprintMismatch)
		require.NoError(t, s.Save(got.Session))
		got, err = s.Open(Request{})
		require.NoError(t, err)
		assert.False(t, got.FingerprintMismatch, "a save rewrites the version")
		assert.Equal(t, diff.FileFingerprintVersion, got.Session.FingerprintVersion)
	})

	t.Run("annotations alone are state and round-trip", func(t *testing.T) {
		repo := newRepo(t)
		s := newTestStore(t, repo)
		opened, err := s.Open(Request{})
		require.NoError(t, err)
		a := annotation.Annotation{File: "b.go", Line: 1, Type: "+", Comment: "why", Kind: "question",
			Status: annotation.StatusOutdated, Delivered: true,
			Anchor: &annotation.Anchor{Line: 1, Type: "+", Content: "two", After: []string{"x"}}}
		opened.Session.Annotations = []annotation.Annotation{a}
		require.NoError(t, s.Save(opened.Session))
		got, err := s.Open(Request{})
		require.NoError(t, err)
		require.True(t, got.Resumed)
		assert.Equal(t, []annotation.Annotation{a}, got.Session.Annotations)
	})

	t.Run("annotation without a file makes the session unreadable", func(t *testing.T) {
		repo := newRepo(t)
		s := newTestStore(t, repo)
		s.warnLog = func(string, ...any) {}
		dir := s.branchDir("feature")
		require.NoError(t, os.MkdirAll(dir, 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "x.json"), []byte(`{"version":1,"annotations":[{"line":1}]}`), 0o600))
		got, err := s.Open(Request{})
		require.NoError(t, err)
		assert.False(t, got.Resumed)
	})

	t.Run("emptied session is still rewritten", func(t *testing.T) {
		repo := newRepo(t)
		s := newTestStore(t, repo)
		opened, err := s.Open(Request{})
		require.NoError(t, err)
		opened.Session.Reviewed["a"] = "1"
		require.NoError(t, s.Save(opened.Session))
		delete(opened.Session.Reviewed, "a")
		require.NoError(t, s.Save(opened.Session))
		got, err := s.Open(Request{})
		require.NoError(t, err)
		assert.True(t, got.Resumed)
		assert.Empty(t, got.Session.Reviewed)
	})

	t.Run("malformed and future files are skipped", func(t *testing.T) {
		repo := newRepo(t)
		s := newTestStore(t, repo)
		var warnings []string
		s.warnLog = func(format string, args ...any) { warnings = append(warnings, format) }
		dir := s.branchDir("feature")
		require.NoError(t, os.MkdirAll(dir, 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "bad.json"), []byte("{"), 0o600))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "future.json"), []byte(`{"version": 99}`), 0o600))
		require.NoError(t, os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("x"), 0o600))
		got, err := s.Open(Request{})
		require.NoError(t, err)
		assert.False(t, got.Resumed)
		assert.Len(t, warnings, 2)
	})

	t.Run("id and branch come from the file location", func(t *testing.T) {
		repo := newRepo(t)
		s := newTestStore(t, repo)
		dir := s.branchDir("feature")
		require.NoError(t, os.MkdirAll(dir, 0o700))
		body := `{"version":1,"id":"../../evil","branch":"other","reviewed":{"a":"1"}}`
		require.NoError(t, os.WriteFile(filepath.Join(dir, "good.json"), []byte(body), 0o600))
		got, err := s.Open(Request{})
		require.NoError(t, err)
		require.True(t, got.Resumed)
		assert.Equal(t, "good", got.Session.ID)
		assert.Equal(t, "feature", got.Session.Branch)
	})

	t.Run("save rejects an incomplete session", func(t *testing.T) {
		s := newTestStore(t, newRepo(t))
		require.Error(t, s.Save(nil))
		require.Error(t, s.Save(&Session{ID: "x"}))
	})
}

func TestEscapeBranch(t *testing.T) {
	assert.Equal(t, "feature%2Fx", escapeBranch("feature/x"))
	assert.Equal(t, "_..", escapeBranch(".."))
	assert.Equal(t, "_", escapeBranch(""))
	assert.Equal(t, "a%25b", escapeBranch("a%b"))
	assert.Equal(t, "%5Ftmp", escapeBranch("_tmp"))
	assert.NotEqual(t, escapeBranch("a/b"), escapeBranch("a_b"))
	for _, b := range []string{"feature/x", "..", ".", "", "a%b", "_tmp", "a_b", "über/ß", "x y"} {
		got, err := unescapeBranch(escapeBranch(b))
		require.NoError(t, err)
		assert.Equal(t, b, got)
	}
	_, err := unescapeBranch("bad%2")
	require.Error(t, err)
	_, err = unescapeBranch("bad%zz")
	require.Error(t, err)
}

// saveSessionOn saves a fresh session for branch with one reviewed mark and
// one annotation on file mark, at the current HEAD, and returns it.
func saveSessionOn(t *testing.T, s *Store, branch, mark string) *Session {
	t.Helper()
	opened, err := s.Open(Request{Branch: branch, Fresh: true})
	require.NoError(t, err)
	opened.Session.Reviewed[mark] = "fp-" + mark
	opened.Session.Annotations = []annotation.Annotation{{File: mark, Line: 1, Type: "+", Comment: "on " + branch}}
	require.NoError(t, s.Save(opened.Session))
	return opened.Session
}

func TestStore_Inherit(t *testing.T) {
	t.Run("a branch cut from a reviewed branch starts from its session", func(t *testing.T) {
		repo := newRepo(t) // on feature
		s := newTestStore(t, repo)
		parent := saveSessionOn(t, s, "feature", "b.txt")
		gitIn(t, repo, "checkout", "-q", "-b", "feature-2")
		commitFile(t, repo, "c.txt", "three\n")

		got, err := s.Open(Request{})
		require.NoError(t, err)
		assert.Equal(t, "feature", got.InheritedFrom)
		assert.True(t, got.Resumed)
		assert.Equal(t, "feature-2", got.Session.Branch)
		assert.NotEqual(t, parent.ID, got.Session.ID)
		assert.Equal(t, map[string]string{"b.txt": "fp-b.txt"}, got.Session.Reviewed)
		require.Len(t, got.Session.Annotations, 1)

		again, err := s.Open(Request{})
		require.NoError(t, err)
		assert.Empty(t, again.InheritedFrom, "the copy was saved, so the next open resumes it")
		assert.Equal(t, got.Session.ID, again.Session.ID)

		orig, err := s.Open(Request{Branch: "feature"})
		require.NoError(t, err)
		assert.Equal(t, parent.ID, orig.Session.ID, "the parent is untouched")
	})

	t.Run("nearest ancestor wins", func(t *testing.T) {
		repo := newRepo(t)
		s := newTestStore(t, repo)
		gitIn(t, repo, "checkout", "-q", "-b", "mid")
		commitFile(t, repo, "m.txt", "m\n")
		saveSessionOn(t, s, "mid", "m.txt")
		gitIn(t, repo, "checkout", "-q", "feature")
		saveSessionOn(t, s, "feature", "b.txt") // newer, but further away
		gitIn(t, repo, "checkout", "-q", "mid")
		gitIn(t, repo, "checkout", "-q", "-b", "leaf")
		commitFile(t, repo, "l.txt", "l\n")

		got, err := s.Open(Request{})
		require.NoError(t, err)
		assert.Equal(t, "mid", got.InheritedFrom)
	})

	t.Run("no ancestor, no inheritance", func(t *testing.T) {
		repo := newRepo(t)
		s := newTestStore(t, repo)
		saveSessionOn(t, s, "feature", "b.txt")
		gitIn(t, repo, "checkout", "-q", "main")
		gitIn(t, repo, "checkout", "-q", "-b", "other")
		commitFile(t, repo, "o.txt", "o\n")
		got, err := s.Open(Request{})
		require.NoError(t, err)
		assert.Empty(t, got.InheritedFrom)
		assert.False(t, got.Resumed)
	})

	t.Run("base branches and explicit requests never inherit", func(t *testing.T) {
		repo := newRepo(t)
		s := newTestStore(t, repo)
		gitIn(t, repo, "checkout", "-q", "main")
		saveSessionOn(t, s, "old", "a.txt") // head is main's commit, an ancestor of main
		got, err := s.Open(Request{})
		require.NoError(t, err)
		assert.Empty(t, got.InheritedFrom, "main does not inherit")

		gitIn(t, repo, "checkout", "-q", "-b", "fresh")
		got, err = s.Open(Request{Fresh: true})
		require.NoError(t, err)
		assert.Empty(t, got.InheritedFrom)
		got, err = s.Open(Request{Name: "x"})
		require.NoError(t, err)
		assert.Empty(t, got.InheritedFrom)
		got, err = s.Open(Request{})
		require.NoError(t, err)
		assert.Equal(t, "old", got.InheritedFrom)
	})
}

func TestStore_ListRenameDelete(t *testing.T) {
	repo := newRepo(t)
	s := newTestStore(t, repo)
	first := saveSessionOn(t, s, "feature", "b.txt")
	second, err := s.Open(Request{Fresh: true})
	require.NoError(t, err)
	second.Session.Annotations = []annotation.Annotation{
		{File: "b.txt", Line: 1, Type: "+", Comment: "x"},
		{File: "b.txt", Line: 2, Type: "+", Comment: "y", Status: annotation.StatusOutdated},
		{File: "b.txt", Line: 3, Type: "+", Comment: "z", Status: annotation.StatusResolved},
	}
	require.NoError(t, s.Save(second.Session))

	list, err := s.List("feature")
	require.NoError(t, err)
	require.Len(t, list, 2)
	assert.Equal(t, second.Session.ID, list[0].ID)
	assert.Equal(t, Summary{ID: second.Session.ID, Updated: list[0].Updated, Open: 1, Outdated: 1, Resolved: 1}, list[0])
	assert.Equal(t, 1, list[1].Reviewed)

	require.NoError(t, s.Rename("feature", first.ID, "  round two  "))
	list, err = s.List("feature")
	require.NoError(t, err)
	assert.Equal(t, second.Session.ID, list[0].ID, "renaming does not make a session the latest")
	assert.Equal(t, "round two", list[1].Name)
	got, err := s.Open(Request{Name: "round two"})
	require.NoError(t, err)
	assert.Equal(t, first.ID, got.Session.ID)

	got, err = s.Open(Request{ID: first.ID})
	require.NoError(t, err)
	assert.Equal(t, first.ID, got.Session.ID)

	require.NoError(t, s.Delete("feature", first.ID))
	list, err = s.List("feature")
	require.NoError(t, err)
	assert.Len(t, list, 1)
	_, err = s.Open(Request{ID: first.ID})
	require.Error(t, err)
	require.Error(t, s.Delete("feature", first.ID))
	require.Error(t, s.Rename("feature", first.ID, "x"))
}

func TestSanitizeName(t *testing.T) {
	assert.Equal(t, "repo", sanitizeName(""))
	assert.Equal(t, "repo", sanitizeName(".."))
	assert.Equal(t, "my_repo", sanitizeName("my repo"))
	assert.Equal(t, "_.._x", sanitizeName("/../x"))
}

func TestDefaultRoot(t *testing.T) {
	t.Setenv("HOME", "/tmp/somewhere")
	assert.Equal(t, "/tmp/somewhere/.config/revdiff/sessions", DefaultRoot())
}
