package refsource

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// gitIn runs git in dir with a fixed identity and returns trimmed stdout.
// extraEnv entries are appended to the environment (e.g. commit dates).
func gitIn(t *testing.T, dir string, extraEnv []string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...) //nolint:gosec // test helper, args constructed internally
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@test.com",
		"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@test.com", "GIT_CONFIG_NOSYSTEM=1")
	cmd.Env = append(cmd.Env, extraEnv...)
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, "git %v: %s", args, out)
	return strings.TrimSpace(string(out))
}

// commitAt writes name=content and commits it with a fixed committer date so
// branch ordering by committerdate is deterministic.
func commitAt(t *testing.T, dir, name, content, date string) string {
	t.Helper()
	require.NoError(t, os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600))
	gitIn(t, dir, nil, "add", name)
	env := []string{"GIT_AUTHOR_DATE=" + date, "GIT_COMMITTER_DATE=" + date}
	gitIn(t, dir, env, "commit", "-q", "-m", "change "+name)
	return gitIn(t, dir, nil, "rev-parse", "HEAD")
}

// newRepo creates a repo on branch main with one commit, plus a feature branch
// with a newer commit, and leaves main checked out.
func newRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	gitIn(t, dir, nil, "-c", "init.defaultBranch=main", "init", "-q")
	commitAt(t, dir, "a.txt", "one\n", "2024-01-01T00:00:00Z")
	gitIn(t, dir, nil, "checkout", "-q", "-b", "feature")
	commitAt(t, dir, "b.txt", "two\n", "2024-02-01T00:00:00Z")
	gitIn(t, dir, nil, "checkout", "-q", "main")
	return dir
}

// fakeGH writes a stub gh script that logs its argv and answers the commands
// the source issues from files in the returned directory.
func fakeGH(t *testing.T, defaultRepo string) (bin, dir string) {
	t.Helper()
	dir = t.TempDir()
	script := `#!/bin/sh
echo "$@" >> "` + dir + `/args.log"
case "$1 $2" in
"repo set-default") printf '%s\n' "` + defaultRepo + `" ;;
"pr list") if [ -f "` + dir + `/list.err" ]; then cat "` + dir + `/list.err" >&2; exit 1; fi; cat "` + dir + `/list.json" ;;
"pr view") cat "` + dir + `/view.json" ;;
esac
`
	bin = filepath.Join(dir, "gh")
	require.NoError(t, os.WriteFile(bin, []byte(script), 0o700)) //nolint:gosec // executable test stub
	return bin, dir
}

func ghLog(t *testing.T, dir string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(dir, "args.log")) //nolint:gosec // test-owned temp path
	require.NoError(t, err)
	return string(data)
}

func TestSource_Branches(t *testing.T) {
	t.Run("local base when no remote", func(t *testing.T) {
		dir := newRepo(t)
		list, err := New(dir).Branches()
		require.NoError(t, err)
		assert.Equal(t, "main", list.Base)
		require.Len(t, list.Branches, 2)
		assert.Equal(t, "feature", list.Branches[0].Name, "most recently committed first")
		assert.Equal(t, "main...feature", list.Branches[0].Ref)
		assert.Equal(t, "change b.txt", list.Branches[0].Subject)
		assert.NotEmpty(t, list.Branches[0].Age)
		assert.False(t, list.Branches[0].Current)
		assert.Equal(t, "main", list.Branches[1].Name)
		assert.Empty(t, list.Branches[1].Ref, "the base branch itself has nothing to compare against")
		assert.True(t, list.Branches[1].Current)
	})

	t.Run("origin HEAD wins and local base becomes reviewable", func(t *testing.T) {
		dir := newRepo(t)
		gitIn(t, dir, nil, "update-ref", "refs/remotes/origin/trunk", "main")
		gitIn(t, dir, nil, "update-ref", "refs/remotes/origin/main", "main")
		gitIn(t, dir, nil, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/trunk")
		list, err := New(dir).Branches()
		require.NoError(t, err)
		assert.Equal(t, "origin/trunk", list.Base)
		refs := map[string]string{}
		for _, b := range list.Branches {
			refs[b.Name] = b.Ref
		}
		assert.Equal(t, map[string]string{"feature": "origin/trunk...feature", "main": "origin/trunk...main"}, refs)
	})

	t.Run("conventional remote names before local", func(t *testing.T) {
		dir := newRepo(t)
		gitIn(t, dir, nil, "update-ref", "refs/remotes/upstream/master", "main")
		list, err := New(dir).Branches()
		require.NoError(t, err)
		assert.Equal(t, "upstream/master", list.Base)

		gitIn(t, dir, nil, "update-ref", "refs/remotes/origin/main", "main")
		list, err = New(dir).Branches()
		require.NoError(t, err)
		assert.Equal(t, "origin/main", list.Base, "origin is preferred over upstream")
	})

	t.Run("no base found leaves refs empty", func(t *testing.T) {
		dir := newRepo(t)
		gitIn(t, dir, nil, "branch", "-q", "-m", "main", "trunk")
		list, err := New(dir).Branches()
		require.NoError(t, err)
		assert.Empty(t, list.Base)
		for _, b := range list.Branches {
			assert.Empty(t, b.Ref, b.Name)
		}
	})

	t.Run("not a repository", func(t *testing.T) {
		_, err := New(t.TempDir()).Branches()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "list branches")
	})
}

func TestSource_PullRequests(t *testing.T) {
	t.Run("lists with repo from gh default", func(t *testing.T) {
		dir := newRepo(t)
		bin, ghDir := fakeGH(t, "acme/widget")
		list := `[{"number":12,"title":"Add thing","headRefName":"thing","baseRefName":"main","isDraft":true,"author":{"login":"alice"}},
			{"number":9,"title":"Fix","headRefName":"fix","baseRefName":"dev","isDraft":false,"author":{"login":"bob"}}]`
		require.NoError(t, os.WriteFile(filepath.Join(ghDir, "list.json"), []byte(list), 0o600))
		s := New(dir)
		s.ghBin = bin

		prs, err := s.PullRequests()
		require.NoError(t, err)
		assert.Equal(t, []PullRequest{
			{Number: 12, Title: "Add thing", Head: "thing", Base: "main", Author: "alice", Draft: true},
			{Number: 9, Title: "Fix", Head: "fix", Base: "dev", Author: "bob"},
		}, prs)
		log := ghLog(t, ghDir)
		assert.Contains(t, log, "pr list --state open --limit 50 --json number,title,headRefName,baseRefName,author,isDraft --repo acme/widget")
	})

	t.Run("falls back to origin url when gh has no default", func(t *testing.T) {
		dir := newRepo(t)
		gitIn(t, dir, nil, "remote", "add", "origin", "git@github.com:acme/widget.git")
		bin, ghDir := fakeGH(t, "")
		require.NoError(t, os.WriteFile(filepath.Join(ghDir, "list.json"), []byte(`[]`), 0o600))
		s := New(dir)
		s.ghBin = bin

		prs, err := s.PullRequests()
		require.NoError(t, err)
		assert.Empty(t, prs)
		assert.Contains(t, ghLog(t, ghDir), "--repo github.com/acme/widget")
	})

	t.Run("no repo hint at all", func(t *testing.T) {
		dir := newRepo(t)
		bin, ghDir := fakeGH(t, "")
		require.NoError(t, os.WriteFile(filepath.Join(ghDir, "list.json"), []byte(`[]`), 0o600))
		s := New(dir)
		s.ghBin = bin
		_, err := s.PullRequests()
		require.NoError(t, err)
		assert.NotContains(t, ghLog(t, ghDir), "--repo")
	})

	t.Run("gh missing", func(t *testing.T) {
		s := New(newRepo(t))
		s.ghBin = filepath.Join(t.TempDir(), "gh")
		_, err := s.PullRequests()
		require.ErrorIs(t, err, ErrGHNotFound)
		_, err = s.PullRequestRef(1)
		require.ErrorIs(t, err, ErrGHNotFound)
	})

	t.Run("gh failure surfaces first stderr line", func(t *testing.T) {
		bin, ghDir := fakeGH(t, "acme/widget")
		require.NoError(t, os.WriteFile(filepath.Join(ghDir, "list.err"), []byte("authentication required\nrun gh auth login\n"), 0o600))
		s := New(newRepo(t))
		s.ghBin = bin
		_, err := s.PullRequests()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "authentication required")
		assert.NotContains(t, err.Error(), "gh auth login")
	})

	t.Run("bad json", func(t *testing.T) {
		bin, ghDir := fakeGH(t, "acme/widget")
		require.NoError(t, os.WriteFile(filepath.Join(ghDir, "list.json"), []byte(`not json`), 0o600))
		s := New(newRepo(t))
		s.ghBin = bin
		_, err := s.PullRequests()
		require.Error(t, err)
		assert.Contains(t, err.Error(), "parse gh pr list output")
	})
}

// prFixture builds a "GitHub" bare repository holding main plus a pull request
// head at refs/pull/7/head, and a local clone of main whose remote URL is a
// github.com URL rewritten (url.insteadOf) to the bare path — so the real fetch
// path runs without network.
type prFixture struct {
	local, bare      string
	baseOid, headOid string
}

func newPRFixture(t *testing.T, remoteName string) prFixture {
	t.Helper()
	work := newRepo(t)
	baseOid := gitIn(t, work, nil, "rev-parse", "main")
	gitIn(t, work, nil, "checkout", "-q", "-b", "pr-head", "main")
	headOid := commitAt(t, work, "pr.txt", "pr\n", "2024-03-01T00:00:00Z")

	bare := filepath.Join(t.TempDir(), "widget.git")
	gitIn(t, work, nil, "init", "-q", "--bare", bare)
	gitIn(t, work, nil, "push", "-q", bare, "main:refs/heads/main", "pr-head:refs/pull/7/head")

	local := t.TempDir()
	gitIn(t, local, nil, "-c", "init.defaultBranch=main", "init", "-q")
	commitAt(t, local, "local.txt", "x\n", "2024-01-05T00:00:00Z")
	remURL := "https://github.com/acme/widget.git"
	gitIn(t, local, nil, "config", "url."+bare+".insteadOf", remURL)
	if remoteName != "" {
		gitIn(t, local, nil, "remote", "add", remoteName, remURL)
	}
	return prFixture{local: local, bare: bare, baseOid: baseOid, headOid: headOid}
}

func (f prFixture) source(t *testing.T) (*Source, string) {
	t.Helper()
	bin, ghDir := fakeGH(t, "acme/widget")
	view := `{"headRefOid":"` + f.headOid + `","baseRefName":"main","baseRefOid":"` + f.baseOid +
		`","url":"https://github.com/acme/widget/pull/7"}`
	require.NoError(t, os.WriteFile(filepath.Join(ghDir, "view.json"), []byte(view), 0o600))
	s := New(f.local)
	s.ghBin = bin
	return s, ghDir
}

func TestSource_PullRequestRef(t *testing.T) {
	t.Run("fetches through matching remote without touching refs", func(t *testing.T) {
		f := newPRFixture(t, "upstream")
		s, ghDir := f.source(t)
		branchesBefore := gitIn(t, f.local, nil, "for-each-ref")

		ref, err := s.PullRequestRef(7)
		require.NoError(t, err)
		assert.Equal(t, f.baseOid+"..."+f.headOid, ref)
		assert.Contains(t, ghLog(t, ghDir), "pr view 7 --json headRefOid,baseRefName,baseRefOid,url --repo acme/widget")

		assert.Equal(t, branchesBefore, gitIn(t, f.local, nil, "for-each-ref"), "no branch or remote-tracking ref may be created")
		assert.Empty(t, gitIn(t, f.local, nil, "status", "--porcelain"), "working tree untouched")
		assert.Equal(t, "main", gitIn(t, f.local, nil, "rev-parse", "--abbrev-ref", "HEAD"))
		gitIn(t, f.local, nil, "cat-file", "-e", f.headOid+"^{commit}")
	})

	t.Run("falls back to repository url without a matching remote", func(t *testing.T) {
		f := newPRFixture(t, "")
		s, _ := f.source(t)
		ref, err := s.PullRequestRef(7)
		require.NoError(t, err)
		assert.Equal(t, f.baseOid+"..."+f.headOid, ref)
	})

	t.Run("fetch failure", func(t *testing.T) {
		f := newPRFixture(t, "upstream")
		s, _ := f.source(t)
		require.NoError(t, os.RemoveAll(f.bare))
		_, err := s.PullRequestRef(7)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "fetch PR #7")
	})

	t.Run("rejects malformed oids", func(t *testing.T) {
		f := newPRFixture(t, "upstream")
		s, ghDir := f.source(t)
		require.NoError(t, os.WriteFile(filepath.Join(ghDir, "view.json"),
			[]byte(`{"headRefOid":"--upload-pack=x","baseRefName":"main","baseRefOid":"`+f.baseOid+`","url":"x"}`), 0o600))
		_, err := s.PullRequestRef(7)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "unexpected commit ids")
	})

	t.Run("invalid number", func(t *testing.T) {
		_, err := New(t.TempDir()).PullRequestRef(0)
		require.Error(t, err)
	})
}

func TestSource_CheckRef(t *testing.T) {
	dir := newRepo(t)
	s := New(dir)
	for _, ref := range []string{"main", "feature", "main...feature", "main..feature", "HEAD~0..", "...feature", " main "} {
		require.NoError(t, s.CheckRef(ref), ref)
	}
	for _, ref := range []string{"", "nope", "main...nope", "-x", "--output=/tmp/x", "main..--x", "main feature"} {
		assert.Error(t, s.CheckRef(ref), ref)
	}
}

func TestSource_ParseRemoteURL(t *testing.T) {
	s := New("")
	tests := []struct {
		in   string
		want remoteRepo
		ok   bool
	}{
		{"https://github.com/acme/widget.git", remoteRepo{"github.com", "acme", "widget"}, true},
		{"https://github.com/acme/widget", remoteRepo{"github.com", "acme", "widget"}, true},
		{"https://token@github.com/acme/widget/", remoteRepo{"github.com", "acme", "widget"}, true},
		{"ssh://git@ghe.corp:2222/team/repo.git", remoteRepo{"ghe.corp", "team", "repo"}, true},
		{"git@github.com:acme/widget.git", remoteRepo{"github.com", "acme", "widget"}, true},
		{"/srv/git/widget.git", remoteRepo{}, false},
		{"https://github.com/acme", remoteRepo{}, false},
		{"https://gitlab.com/group/sub/repo.git", remoteRepo{}, false},
		{"", remoteRepo{}, false},
	}
	for _, tc := range tests {
		got, ok := s.parseRemoteURL(tc.in)
		assert.Equal(t, tc.ok, ok, tc.in)
		assert.Equal(t, tc.want, got, tc.in)
	}
}
