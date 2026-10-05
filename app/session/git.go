package session

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const gitTimeout = 10 * time.Second

// gitRunner runs read-only git commands in one repository. Commands run with
// cmd.Dir set, a timeout, no shell, a null stdin and prompts disabled.
type gitRunner struct {
	dir string
}

// repoIdentity returns a stable hash identifying the repository and a readable
// name for its directory. The identity is the normalized origin URL when the
// repository has one, so separate clones and worktrees of one project share
// sessions; otherwise the absolute git common dir, which every worktree of one
// clone shares.
func (g gitRunner) repoIdentity() (id, name string, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), gitTimeout)
	defer cancel()

	var identity string
	if out, uerr := g.run(ctx, "config", "--get", "remote.origin.url"); uerr == nil {
		if u := normalizeRemoteURL(strings.TrimSpace(out)); u != "" {
			identity = "remote:" + u
			name = strings.TrimSuffix(filepath.Base(u), ".git")
		}
	}
	if identity == "" {
		common, cerr := g.run(ctx, "rev-parse", "--git-common-dir")
		if cerr != nil {
			return "", "", fmt.Errorf("resolve repository: %w", cerr)
		}
		common = strings.TrimSpace(common)
		if !filepath.IsAbs(common) {
			common = filepath.Join(g.dir, common)
		}
		if resolved, rerr := filepath.EvalSymlinks(common); rerr == nil {
			common = resolved
		}
		common = filepath.Clean(common)
		identity = "path:" + common
		name = filepath.Base(common)
		if name == ".git" {
			name = filepath.Base(filepath.Dir(common))
		}
		name = strings.TrimSuffix(name, ".git")
	}
	sum := sha256.Sum256([]byte(identity))
	return hex.EncodeToString(sum[:]), name, nil
}

// normalizeRemoteURL reduces the common remote URL shapes (https, ssh://, and
// scp-like user@host:path) to "host/path" with a lowercase host and no ".git"
// suffix, so https and ssh clones of one repository produce the same identity.
// Local paths and file:// URLs are returned cleaned. Returns "" for empty input.
func normalizeRemoteURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	var host, path string
	switch {
	case strings.HasPrefix(raw, "file://"):
		return filepath.Clean(strings.TrimPrefix(raw, "file://"))
	case strings.Contains(raw, "://"):
		_, rest, _ := strings.Cut(raw, "://")
		host, path, _ = strings.Cut(rest, "/")
		if _, h, ok := strings.Cut(host, "@"); ok {
			host = h
		}
		if h, _, ok := strings.Cut(host, ":"); ok {
			host = h
		}
	case strings.HasPrefix(raw, "/") || strings.HasPrefix(raw, "."):
		return filepath.Clean(raw)
	case strings.Contains(raw, ":"):
		host, path, _ = strings.Cut(raw, ":")
		if _, h, ok := strings.Cut(host, "@"); ok {
			host = h
		}
	default:
		return filepath.Clean(raw)
	}
	path = strings.TrimSuffix(strings.Trim(path, "/"), ".git")
	if host == "" {
		return path
	}
	return strings.ToLower(host) + "/" + path
}

// branchKey derives the branch a review of ref belongs to. Working-tree,
// staged and single-ref reviews belong to the checked-out branch. A range
// "A..B" / "A...B" belongs to B: a local branch name as is, a remote-tracking
// ref ("origin/feature") as its branch name, HEAD or an empty side as the
// checked-out branch, and anything else (a commit id, a tag) literally.
func (g gitRunner) branchKey(ref string) string {
	ctx, cancel := context.WithTimeout(context.Background(), gitTimeout)
	defer cancel()

	right, isRange := rangeTip(ref)
	if !isRange || right == "" || right == "HEAD" {
		return g.currentBranch(ctx)
	}
	if g.refExists(ctx, "refs/heads/"+right) {
		return right
	}
	if out, err := g.run(ctx, "remote"); err == nil {
		for remote := range strings.FieldsSeq(out) {
			if b, ok := strings.CutPrefix(right, remote+"/"); ok && b != "" && g.refExists(ctx, "refs/remotes/"+right) {
				return b
			}
		}
	}
	return right
}

// currentBranch returns the checked-out branch, or "detached-<short sha>" for a
// detached HEAD, or "detached" when even HEAD cannot be resolved.
func (g gitRunner) currentBranch(ctx context.Context) string {
	if out, err := g.run(ctx, "symbolic-ref", "--quiet", "--short", "HEAD"); err == nil {
		if b := strings.TrimSpace(out); b != "" {
			return b
		}
	}
	if out, err := g.run(ctx, "rev-parse", "--short", "HEAD"); err == nil {
		if sha := strings.TrimSpace(out); sha != "" {
			return "detached-" + sha
		}
	}
	return "detached"
}

// tipCommit resolves the commit at the tip of ref: the right side of a range,
// else HEAD. Returns "" when it does not resolve (e.g. an empty repository).
func (g gitRunner) tipCommit(ref string) string {
	ctx, cancel := context.WithTimeout(context.Background(), gitTimeout)
	defer cancel()
	tip := "HEAD"
	if right, ok := rangeTip(ref); ok && right != "" {
		tip = right
	}
	if strings.HasPrefix(tip, "-") {
		return ""
	}
	out, err := g.run(ctx, "rev-parse", "--verify", "--quiet", tip+"^{commit}")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(out)
}

// ancestorDistance reports whether commit from is an ancestor of (or equal to)
// commit to and, if so, how many commits lie between them.
func (g gitRunner) ancestorDistance(from, to string) (int, bool) {
	if from == "" || to == "" || strings.HasPrefix(from, "-") || strings.HasPrefix(to, "-") {
		return 0, false
	}
	ctx, cancel := context.WithTimeout(context.Background(), gitTimeout)
	defer cancel()
	if _, err := g.run(ctx, "merge-base", "--is-ancestor", from, to); err != nil {
		return 0, false
	}
	out, err := g.run(ctx, "rev-list", "--count", from+".."+to)
	if err != nil {
		return 0, false
	}
	n, err := strconv.Atoi(strings.TrimSpace(out))
	if err != nil {
		return 0, false
	}
	return n, true
}

// isBaseBranch reports whether branch is one of the repository's base
// branches: main, master, or the branch origin's HEAD points at.
func (g gitRunner) isBaseBranch(branch string) bool {
	if branch == "main" || branch == "master" {
		return true
	}
	ctx, cancel := context.WithTimeout(context.Background(), gitTimeout)
	defer cancel()
	out, err := g.run(ctx, "symbolic-ref", "--quiet", "--short", "refs/remotes/origin/HEAD")
	if err != nil {
		return false
	}
	return strings.TrimPrefix(strings.TrimSpace(out), "origin/") == branch
}

func (g gitRunner) refExists(ctx context.Context, ref string) bool {
	_, err := g.run(ctx, "show-ref", "--verify", "--quiet", ref)
	return err == nil
}

// rangeTip returns the right side of "A...B" or "A..B"; ok is false for a
// single ref.
func rangeTip(ref string) (right string, ok bool) {
	if _, r, found := strings.Cut(ref, "..."); found {
		return r, true
	}
	if _, r, found := strings.Cut(ref, ".."); found {
		return r, true
	}
	return "", false
}

func (g gitRunner) run(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...) //nolint:gosec // fixed binary, args built internally
	cmd.Dir = g.dir
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0", "GIT_OPTIONAL_LOCKS=0")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		if ctx.Err() != nil {
			return "", fmt.Errorf("git %s: %w", args[0], ctx.Err())
		}
		msg, _, _ := strings.Cut(strings.TrimSpace(stderr.String()), "\n")
		if msg == "" {
			return "", fmt.Errorf("git %s: %w", args[0], err)
		}
		return "", fmt.Errorf("git %s: %w: %s", args[0], errGit, msg)
	}
	return string(out), nil
}

var errGit = errors.New("git failed")
