// Package refsource lists and resolves the review targets offered by revdiff's
// runtime ref switcher: local git branches, open GitHub pull requests (through
// the gh CLI), and arbitrary user-typed refs. It owns every git/gh process the
// feature runs, so app/ui stays free of exec concerns and consumes it through
// the consumer-side ui.RefSource interface.
//
// Every target resolves to a revdiff ref string with GitHub's three-dot
// semantics: a branch becomes "<base>...<branch>" and a pull request becomes
// "<baseOid>...<headOid>", so the diff shows only what changed on the branch
// since it forked from the base, not what changed on the base meanwhile.
//
// Commands always run with cmd.Dir set to the repository work dir, a context
// timeout, no shell, a closed stdin, and prompts disabled (GIT_TERMINAL_PROMPT=0,
// GH_PROMPT_DISABLED=1), so a missing credential or an ambiguous gh default
// repository fails fast instead of hanging the TUI. Resolving a pull request
// fetches its commits without creating or moving any local branch and without
// touching the working tree or index: only FETCH_HEAD and the object database
// change (--refmap= stops git from opportunistically updating the remote's
// tracking refs when fetching through a named remote).
package refsource

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// ErrGHNotFound is returned by PullRequests and PullRequestRef when the gh CLI
// is not on PATH. The switcher treats it as a non-fatal reason to hide pull
// requests; branches and typed refs keep working.
var ErrGHNotFound = errors.New("gh CLI not found on PATH")

const (
	gitTimeout   = 10 * time.Second
	ghTimeout    = 30 * time.Second
	fetchTimeout = 2 * time.Minute
	prListLimit  = 50
)

// Branch is one local branch offered by the switcher.
type Branch struct {
	Name    string // short branch name, e.g. "feature/x"
	Ref     string // revdiff ref for reviewing the branch ("<base>...<name>"); empty when Name is the base itself
	Current bool   // true for the checked-out branch
	Age     string // relative committer date of the branch tip, e.g. "3 days ago"
	Subject string // subject of the branch tip commit
}

// BranchList is the local branch set plus the base the three-dot refs compare against.
type BranchList struct {
	Base     string   // default base, e.g. "origin/main"; empty when none could be found
	Branches []Branch // most recently committed first
}

// PullRequest is one open pull request reported by gh.
type PullRequest struct {
	Number int
	Title  string
	Head   string // head branch name
	Base   string // base branch name
	Author string // author login
	Draft  bool
}

// Source runs git and gh in one repository. The zero value is not usable; use New.
type Source struct {
	dir   string
	ghBin string // gh executable name or path; tests point it at a stub
}

// New returns a Source operating on the git work tree at dir.
func New(dir string) *Source {
	return &Source{dir: dir, ghBin: "gh"}
}

// Branches lists local branches, most recently committed first, each with the
// three-dot ref that reviews it against the default base branch.
func (s *Source) Branches() (BranchList, error) {
	ctx, cancel := context.WithTimeout(context.Background(), gitTimeout)
	defer cancel()

	base := s.baseBranch(ctx)
	out, err := s.git(ctx, "for-each-ref", "--sort=-committerdate",
		"--format=%(refname:short)%00%(HEAD)%00%(committerdate:relative)%00%(subject)", "refs/heads")
	if err != nil {
		return BranchList{Base: base}, fmt.Errorf("list branches: %w", err)
	}
	list := BranchList{Base: base}
	for line := range strings.SplitSeq(strings.TrimRight(out, "\n"), "\n") {
		fields := strings.Split(line, "\x00")
		if len(fields) < 4 || fields[0] == "" {
			continue
		}
		b := Branch{Name: fields[0], Current: fields[1] == "*", Age: fields[2], Subject: fields[3]}
		if base != "" && b.Name != base {
			b.Ref = base + "..." + b.Name
		}
		list.Branches = append(list.Branches, b)
	}
	return list, nil
}

// baseBranch picks the branch three-dot refs compare against: the symbolic
// target of a remote's HEAD when present, else the first existing of the
// conventional main/master names. origin is checked before upstream, and remote
// branches before local ones because a local main is often stale. Returns ""
// when none exist.
func (s *Source) baseBranch(ctx context.Context) string {
	for _, remote := range []string{"origin", "upstream"} {
		if out, err := s.git(ctx, "symbolic-ref", "--quiet", "--short", "refs/remotes/"+remote+"/HEAD"); err == nil {
			if b := strings.TrimSpace(out); b != "" {
				return b
			}
		}
		for _, name := range []string{"main", "master"} {
			if s.refExists(ctx, "refs/remotes/"+remote+"/"+name) {
				return remote + "/" + name
			}
		}
	}
	for _, name := range []string{"main", "master"} {
		if s.refExists(ctx, "refs/heads/"+name) {
			return name
		}
	}
	return ""
}

func (s *Source) refExists(ctx context.Context, ref string) bool {
	_, err := s.git(ctx, "show-ref", "--verify", "--quiet", ref)
	return err == nil
}

// PullRequests lists open pull requests through gh. Returns ErrGHNotFound when
// gh is unavailable; any other gh failure (not authenticated, no GitHub remote,
// network) is returned as an error carrying gh's message.
func (s *Source) PullRequests() ([]PullRequest, error) {
	if _, err := exec.LookPath(s.ghBin); err != nil {
		return nil, ErrGHNotFound
	}
	ctx, cancel := context.WithTimeout(context.Background(), ghTimeout)
	defer cancel()

	args := []string{"pr", "list", "--state", "open", "--limit", strconv.Itoa(prListLimit),
		"--json", "number,title,headRefName,baseRefName,author,isDraft"}
	args = append(args, s.repoArgs(ctx)...)
	out, err := s.gh(ctx, args...)
	if err != nil {
		return nil, err
	}
	var raw []struct {
		Number      int    `json:"number"`
		Title       string `json:"title"`
		HeadRefName string `json:"headRefName"`
		BaseRefName string `json:"baseRefName"`
		IsDraft     bool   `json:"isDraft"`
		Author      struct {
			Login string `json:"login"`
		} `json:"author"`
	}
	if err := json.Unmarshal([]byte(out), &raw); err != nil {
		return nil, fmt.Errorf("parse gh pr list output: %w", err)
	}
	prs := make([]PullRequest, 0, len(raw))
	for _, r := range raw {
		prs = append(prs, PullRequest{Number: r.Number, Title: r.Title, Head: r.HeadRefName,
			Base: r.BaseRefName, Author: r.Author.Login, Draft: r.IsDraft})
	}
	return prs, nil
}

var oidPattern = regexp.MustCompile(`^[0-9a-f]{40}([0-9a-f]{24})?$`)

// PullRequestRef fetches the commits of pull request number and returns the
// GitHub-equivalent revdiff ref "<baseOid>...<headOid>". The fetch reads
// refs/pull/<n>/head and the base branch into FETCH_HEAD only — no local branch
// is created or moved and the working tree is untouched. It prefers a configured
// remote pointing at the pull request's repository (so the user's credentials
// and URL rewrites apply) and otherwise fetches from the repository URL gh reports.
func (s *Source) PullRequestRef(number int) (string, error) {
	if number <= 0 {
		return "", fmt.Errorf("invalid pull request number %d", number)
	}
	if _, err := exec.LookPath(s.ghBin); err != nil {
		return "", ErrGHNotFound
	}
	ctx, cancel := context.WithTimeout(context.Background(), fetchTimeout)
	defer cancel()

	repoArgs := s.repoArgs(ctx)
	args := append([]string{"pr", "view", strconv.Itoa(number), "--json", "headRefOid,baseRefName,baseRefOid,url"}, repoArgs...)
	out, err := s.gh(ctx, args...)
	if err != nil {
		return "", err
	}
	var pr struct {
		HeadRefOid  string `json:"headRefOid"`
		BaseRefName string `json:"baseRefName"`
		BaseRefOid  string `json:"baseRefOid"`
		URL         string `json:"url"`
	}
	if err := json.Unmarshal([]byte(out), &pr); err != nil {
		return "", fmt.Errorf("parse gh pr view output: %w", err)
	}
	if !oidPattern.MatchString(pr.HeadRefOid) || !oidPattern.MatchString(pr.BaseRefOid) {
		return "", fmt.Errorf("gh reported unexpected commit ids for PR #%d", number)
	}

	src := s.fetchSource(ctx, pr.URL, number)
	if src == "" {
		return "", fmt.Errorf("cannot determine where to fetch PR #%d from", number)
	}
	refspecs := []string{"refs/pull/" + strconv.Itoa(number) + "/head"}
	if pr.BaseRefName != "" && !strings.HasPrefix(pr.BaseRefName, "-") {
		refspecs = append(refspecs, "refs/heads/"+pr.BaseRefName)
	}
	if _, err := s.git(ctx, append([]string{"fetch", "--no-tags", "--quiet", "--refmap=", "--", src}, refspecs...)...); err != nil {
		return "", fmt.Errorf("fetch PR #%d: %w", number, err)
	}
	// the base branch may have moved between gh pr view and the fetch; ask for
	// the exact base commit gh reported (GitHub serves reachable commits by id).
	if !s.commitExists(ctx, pr.BaseRefOid) {
		if _, err := s.git(ctx, "fetch", "--no-tags", "--quiet", "--refmap=", "--", src, pr.BaseRefOid); err != nil {
			return "", fmt.Errorf("fetch base of PR #%d: %w", number, err)
		}
	}
	for _, oid := range []string{pr.HeadRefOid, pr.BaseRefOid} {
		if !s.commitExists(ctx, oid) {
			return "", fmt.Errorf("commit %s of PR #%d is not available after fetch", oid[:12], number)
		}
	}
	return pr.BaseRefOid + "..." + pr.HeadRefOid, nil
}

func (s *Source) commitExists(ctx context.Context, oid string) bool {
	_, err := s.git(ctx, "cat-file", "-e", oid+"^{commit}")
	return err == nil
}

// CheckRef verifies that a user-typed ref resolves to commits before the
// switcher drops the current review for it. Accepts a single ref or an
// "A..B" / "A...B" range (an empty side means HEAD, as in git). Endpoints that
// start with "-" are rejected so typed text can never be read as a git option.
func (s *Source) CheckRef(ref string) error {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return errors.New("empty ref")
	}
	endpoints := []string{ref}
	if left, right, ok := strings.Cut(ref, "..."); ok {
		endpoints = []string{left, right}
	} else if left, right, ok := strings.Cut(ref, ".."); ok {
		endpoints = []string{left, right}
	}
	ctx, cancel := context.WithTimeout(context.Background(), gitTimeout)
	defer cancel()
	for _, ep := range endpoints {
		if ep == "" {
			continue // git treats an empty range side as HEAD
		}
		if strings.HasPrefix(ep, "-") || strings.ContainsAny(ep, " \t\n") {
			return fmt.Errorf("invalid ref %q", ep)
		}
		if _, err := s.git(ctx, "rev-parse", "--verify", "--quiet", ep+"^{commit}"); err != nil {
			return fmt.Errorf("unknown ref %q", ep)
		}
	}
	return nil
}

// repoArgs returns the --repo argument pinning gh to one repository, so gh never
// has to guess (and never prompts) in a clone with several GitHub remotes such
// as a fork. The repository gh's own default resolves to (gh repo set-default)
// wins; otherwise origin's URL is used. Returns nil when neither is known, in
// which case gh applies its own resolution and reports any ambiguity as an error.
func (s *Source) repoArgs(ctx context.Context) []string {
	if out, err := s.gh(ctx, "repo", "set-default", "--view"); err == nil {
		if repo := strings.TrimSpace(out); repo != "" && !strings.ContainsAny(repo, " \t\n") {
			return []string{"--repo", repo}
		}
	}
	out, err := s.git(ctx, "remote", "get-url", "origin")
	if err != nil {
		return nil
	}
	if r, ok := s.parseRemoteURL(strings.TrimSpace(out)); ok {
		return []string{"--repo", r.host + "/" + r.owner + "/" + r.name}
	}
	return nil
}

// fetchSource picks the git fetch source for a pull request: the first
// configured remote whose URL points at the pull request's repository, else the
// repository URL derived from the pull request URL. Returns "" when neither is
// available.
func (s *Source) fetchSource(ctx context.Context, prURL string, number int) string {
	repoURL := strings.TrimSuffix(prURL, "/pull/"+strconv.Itoa(number))
	if repoURL == prURL {
		repoURL = ""
	}
	want, ok := s.parseRemoteURL(repoURL)
	if !ok {
		return ""
	}
	if out, err := s.git(ctx, "config", "--get-regexp", `^remote\..*\.url$`); err == nil {
		for line := range strings.SplitSeq(strings.TrimSpace(out), "\n") {
			key, url, found := strings.Cut(line, " ")
			if !found {
				continue
			}
			name := strings.TrimSuffix(strings.TrimPrefix(key, "remote."), ".url")
			if got, ok := s.parseRemoteURL(url); ok && got.sameRepo(want) && !strings.HasPrefix(name, "-") {
				return name
			}
		}
	}
	return repoURL + ".git"
}

type remoteRepo struct {
	host, owner, name string
}

func (r remoteRepo) sameRepo(o remoteRepo) bool {
	return strings.EqualFold(r.host, o.host) && strings.EqualFold(r.owner, o.owner) && strings.EqualFold(r.name, o.name)
}

// parseRemoteURL extracts host/owner/name from the common git remote URL
// shapes: https://host/owner/name(.git), ssh://user@host[:port]/owner/name(.git),
// and scp-like user@host:owner/name(.git).
func (s *Source) parseRemoteURL(raw string) (remoteRepo, bool) {
	var host, path string
	switch {
	case strings.Contains(raw, "://"):
		_, rest, _ := strings.Cut(raw, "://")
		host, path, _ = strings.Cut(rest, "/")
		if _, h, ok := strings.Cut(host, "@"); ok {
			host = h
		}
		if h, _, ok := strings.Cut(host, ":"); ok {
			host = h
		}
	case strings.Contains(raw, ":"):
		host, path, _ = strings.Cut(raw, ":")
		if _, h, ok := strings.Cut(host, "@"); ok {
			host = h
		}
	default:
		return remoteRepo{}, false
	}
	path = strings.TrimSuffix(strings.Trim(path, "/"), ".git")
	owner, name, ok := strings.Cut(path, "/")
	if !ok || host == "" || owner == "" || name == "" || strings.Contains(name, "/") {
		return remoteRepo{}, false
	}
	return remoteRepo{host: host, owner: owner, name: name}, true
}

func (s *Source) git(ctx context.Context, args ...string) (string, error) {
	return s.run(ctx, "git", args...)
}

func (s *Source) gh(ctx context.Context, args ...string) (string, error) {
	return s.run(ctx, s.ghBin, args...)
}

// run executes bin with args in the repository directory without a shell. stdin
// is left nil (connected to the null device) and prompts are disabled through
// the environment so neither git nor gh can block waiting for input.
func (s *Source) run(ctx context.Context, bin string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, bin, args...) //nolint:gosec // fixed binaries, args built internally; typed refs are validated by CheckRef
	cmd.Dir = s.dir
	cmd.Env = append(os.Environ(),
		"GIT_TERMINAL_PROMPT=0",
		"GH_PROMPT_DISABLED=1",
		"GH_NO_UPDATE_NOTIFIER=1",
		"NO_COLOR=1",
	)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if ctx.Err() != nil {
			return "", fmt.Errorf("%s %s: %w", bin, firstArg(args), ctx.Err())
		}
		msg, _, _ := strings.Cut(strings.TrimSpace(stderr.String()), "\n")
		if msg == "" {
			msg = err.Error()
		}
		return "", fmt.Errorf("%s %s: %s", bin, firstArg(args), msg)
	}
	return string(out), nil
}

func firstArg(args []string) string {
	if len(args) == 0 {
		return ""
	}
	return args[0]
}
