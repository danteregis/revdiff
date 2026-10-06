package main

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/umputun/revdiff/app/annotation"
	"github.com/umputun/revdiff/app/session"
)

func TestParseArgs_PrintAnnotations(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "unset", args: nil, want: ""},
		{name: "bare defaults to pending", args: []string{"--print-annotations"}, want: printPending},
		{name: "explicit pending", args: []string{"--print-annotations=pending"}, want: printPending},
		{name: "all", args: []string{"--print-annotations=all"}, want: printAll},
		{name: "with ref", args: []string{"--print-annotations", "main"}, want: printPending},
		{name: "with named session", args: []string{"--print-annotations", "--session=alpha"}, want: printPending},
		{name: "with output", args: []string{"--print-annotations=all", "-o", "/tmp/out.md"}, want: printAll},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			opts, err := parseArgs(append(noConfigArgs(t), tt.args...))
			require.NoError(t, err)
			assert.Equal(t, tt.want, opts.PrintAnnotations)
		})
	}
}

func TestParseArgs_PrintAnnotationsInvalidValue(t *testing.T) {
	_, err := parseArgs(append(noConfigArgs(t), "--print-annotations=some"))
	require.Error(t, err)
}

func TestParseArgs_PrintAnnotationsConflicts(t *testing.T) {
	file := filepath.Join(t.TempDir(), "f.txt")
	require.NoError(t, os.WriteFile(file, []byte("x\n"), 0o600))
	tests := []struct {
		args    []string
		wantErr string
	}{
		{[]string{"--stdin"}, "--print-annotations cannot be used with --stdin"},
		{[]string{"--compare-old", file, "--compare-new", file}, "--compare-old/--compare-new"},
		{[]string{"--all-files"}, "--print-annotations cannot be used with --all-files"},
		{[]string{"--only", "a.go"}, "--print-annotations cannot be used with --only"},
		{[]string{"--annotations", file}, "--print-annotations cannot be used with --annotations"},
		{[]string{"--no-session"}, "--print-annotations cannot be used with --no-session"},
		{[]string{"--session=new"}, "--print-annotations cannot be used with --session=new"},
		{[]string{"--dump-config"}, "--print-annotations cannot be used with --dump-config"},
		{[]string{"--dump-keys"}, "--print-annotations cannot be used with --dump-keys"},
		{[]string{"--list-themes"}, "--print-annotations cannot be used with --list-themes"},
	}
	for _, tt := range tests {
		t.Run(tt.wantErr, func(t *testing.T) {
			_, err := parseArgs(append(noConfigArgs(t), append([]string{"--print-annotations"}, tt.args...)...))
			require.Error(t, err)
			assert.Contains(t, err.Error(), tt.wantErr)
		})
	}
}

// printTestRepo creates a git repository with a.go committed and then changed
// in the working tree (line 4 added), and makes it the working directory.
func printTestRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	runMainTestGit(t, repo, "init", "-q")
	runMainTestGit(t, repo, "config", "user.email", "test@example.com")
	runMainTestGit(t, repo, "config", "user.name", "test")
	require.NoError(t, os.WriteFile(filepath.Join(repo, "a.go"), []byte("package a\n\nfunc A() {}\n"), 0o600))
	runMainTestGit(t, repo, "add", "a.go")
	runMainTestGit(t, repo, "commit", "-qm", "initial")
	require.NoError(t, os.WriteFile(filepath.Join(repo, "a.go"), []byte("package a\n\nfunc A() {}\nfunc B() {}\n"), 0o600))
	t.Chdir(repo)
	return repo
}

// saveTestSession stores a new session (or the named one) of the checked-out
// branch with anns.
func saveTestSession(t *testing.T, root, repo, name string, anns []annotation.Annotation) {
	t.Helper()
	st, err := session.New(root, repo)
	require.NoError(t, err)
	opened, err := st.Open(session.Request{Name: name, Fresh: name == ""})
	require.NoError(t, err)
	opened.Session.Annotations = anns
	require.NoError(t, st.Save(opened.Session))
}

// loadTestSession returns the annotations of the session a plain or named
// launch resumes.
func loadTestSession(t *testing.T, root, repo, name string) map[string]annotation.Annotation {
	t.Helper()
	st, err := session.New(root, repo)
	require.NoError(t, err)
	opened, err := st.Open(session.Request{Name: name})
	require.NoError(t, err)
	require.True(t, opened.Resumed)
	out := map[string]annotation.Annotation{}
	for _, a := range opened.Session.Annotations {
		out[a.File+":"+a.Comment] = a
	}
	return out
}

func printArgs(t *testing.T, args ...string) options {
	t.Helper()
	opts, err := parseArgs(append(noConfigArgs(t), args...))
	require.NoError(t, err)
	return opts
}

func TestPrintAnnotations_PendingThenAll(t *testing.T) {
	repo := printTestRepo(t)
	root := t.TempDir()
	saveTestSession(t, root, repo, "", []annotation.Annotation{
		{File: "a.go", Line: 4, Type: "+", Comment: "new"},
		{File: "a.go", Line: 1, Type: " ", Comment: "sent", Delivered: true},
		{File: "a.go", Line: 3, Type: "+", Comment: "code changed",
			Anchor: &annotation.Anchor{Line: 3, Type: "+", Content: "func Gone() {}"}},
		{File: "a.go", Line: 2, Type: " ", Comment: "done", Status: annotation.StatusResolved},
		{File: "c.go", Line: 1, Type: "+", Comment: "file left the diff"},
	})

	var out, errOut bytes.Buffer
	code, err := printAnnotations(printArgs(t, "--print-annotations"), root, &out, &errOut)
	require.NoError(t, err)
	assert.Equal(t, 0, code)
	assert.Equal(t, "## a.go:4 (+)\nnew\n", out.String(), "only pending annotations still on current code")

	saved := loadTestSession(t, root, repo, "")
	assert.True(t, saved["a.go:new"].Delivered, "printed annotations are marked delivered")
	require.NotNil(t, saved["a.go:new"].Anchor, "re-anchoring captured an anchor")
	assert.Equal(t, annotation.StatusOutdated, saved["a.go:code changed"].Status, "lost anchor is outdated")
	assert.False(t, saved["a.go:code changed"].Delivered)
	assert.Equal(t, annotation.StatusOutdated, saved["c.go:file left the diff"].Status)
	assert.Equal(t, annotation.StatusResolved, saved["a.go:done"].Status)

	out.Reset()
	code, err = printAnnotations(printArgs(t, "--print-annotations=pending"), root, &out, &errOut)
	require.NoError(t, err)
	assert.Equal(t, 0, code)
	assert.Empty(t, out.String(), "a second pending print has nothing new")

	out.Reset()
	code, err = printAnnotations(printArgs(t, "--print-annotations=all"), root, &out, &errOut)
	require.NoError(t, err)
	assert.Equal(t, 0, code)
	assert.Equal(t, "## a.go:1 ( )\nsent\n\n## a.go:4 (+)\nnew\n", out.String(),
		"all re-sends every open annotation, never outdated or resolved")
}

func TestPrintAnnotations_AllMarksPendingDelivered(t *testing.T) {
	repo := printTestRepo(t)
	root := t.TempDir()
	saveTestSession(t, root, repo, "", []annotation.Annotation{{File: "a.go", Line: 4, Type: "+", Comment: "new"}})

	var out bytes.Buffer
	_, err := printAnnotations(printArgs(t, "--print-annotations=all"), root, &out, &bytes.Buffer{})
	require.NoError(t, err)
	assert.Equal(t, "## a.go:4 (+)\nnew\n", out.String())
	assert.True(t, loadTestSession(t, root, repo, "")["a.go:new"].Delivered)

	out.Reset()
	_, err = printAnnotations(printArgs(t, "--print-annotations"), root, &out, &bytes.Buffer{})
	require.NoError(t, err)
	assert.Empty(t, out.String())
}

func TestPrintAnnotations_NamedSession(t *testing.T) {
	repo := printTestRepo(t)
	root := t.TempDir()
	saveTestSession(t, root, repo, "alpha", []annotation.Annotation{{File: "a.go", Line: 4, Type: "+", Comment: "from alpha"}})
	saveTestSession(t, root, repo, "", []annotation.Annotation{{File: "a.go", Line: 4, Type: "+", Comment: "from latest"}})

	var out bytes.Buffer
	_, err := printAnnotations(printArgs(t, "--print-annotations"), root, &out, &bytes.Buffer{})
	require.NoError(t, err)
	assert.Equal(t, "## a.go:4 (+)\nfrom latest\n", out.String(), "a plain print uses the latest session")

	out.Reset()
	_, err = printAnnotations(printArgs(t, "--print-annotations", "--session=alpha"), root, &out, &bytes.Buffer{})
	require.NoError(t, err)
	assert.Equal(t, "## a.go:4 (+)\nfrom alpha\n", out.String())
	assert.True(t, loadTestSession(t, root, repo, "alpha")["a.go:from alpha"].Delivered)

	_, err = printAnnotations(printArgs(t, "--print-annotations", "--session=missing"), root, &out, &bytes.Buffer{})
	require.ErrorContains(t, err, `review session "missing" not found`)
}

func TestPrintAnnotations_NoSession(t *testing.T) {
	printTestRepo(t)
	root := t.TempDir()
	var out, errOut bytes.Buffer
	code, err := printAnnotations(printArgs(t, "--print-annotations"), root, &out, &errOut)
	require.NoError(t, err)
	assert.Equal(t, 0, code)
	assert.Empty(t, out.String())
	assert.Contains(t, errOut.String(), "no review session")
	entries, err := os.ReadDir(root)
	if !errors.Is(err, os.ErrNotExist) {
		require.NoError(t, err)
		assert.Empty(t, entries, "printing never writes an empty session")
	}
}

func TestPrintAnnotations_ExitCodeAndOutputFile(t *testing.T) {
	repo := printTestRepo(t)
	root := t.TempDir()
	saveTestSession(t, root, repo, "", []annotation.Annotation{{File: "a.go", Line: 4, Type: "+", Comment: "new"}})
	outFile := filepath.Join(t.TempDir(), "out.md")

	var out bytes.Buffer
	code, err := printAnnotations(printArgs(t, "--print-annotations", "--exit-code-on-annotations", "-o", outFile), root, &out, &bytes.Buffer{})
	require.NoError(t, err)
	assert.Equal(t, exitCodeAnnotations, code)
	assert.Empty(t, out.String(), "-o redirects the output")
	got, err := os.ReadFile(outFile) //nolint:gosec // test reads a file under t.TempDir
	require.NoError(t, err)
	assert.Equal(t, "## a.go:4 (+)\nnew\n", string(got))

	code, err = printAnnotations(printArgs(t, "--print-annotations", "--exit-code-on-annotations", "-o", outFile), root, &out, &bytes.Buffer{})
	require.NoError(t, err)
	assert.Equal(t, 0, code, "nothing pending exits 0")
	got, err = os.ReadFile(outFile) //nolint:gosec // test reads a file under t.TempDir
	require.NoError(t, err)
	assert.Empty(t, string(got), "the output file is rewritten empty, never left stale")
}

func TestPrintAnnotations_FailedOutputIsNotDelivered(t *testing.T) {
	repo := printTestRepo(t)
	root := t.TempDir()
	saveTestSession(t, root, repo, "", []annotation.Annotation{{File: "a.go", Line: 4, Type: "+", Comment: "new"}})

	_, err := printAnnotations(printArgs(t, "--print-annotations", "-o", filepath.Join(t.TempDir(), "missing", "out.md")),
		root, &bytes.Buffer{}, &bytes.Buffer{})
	require.Error(t, err)
	assert.False(t, loadTestSession(t, root, repo, "")["a.go:new"].Delivered)
}

func TestPrintAnnotations_IgnoresIncludeExclude(t *testing.T) {
	repo := printTestRepo(t)
	root := t.TempDir()
	saveTestSession(t, root, repo, "", []annotation.Annotation{{File: "a.go", Line: 4, Type: "+", Comment: "new"}})

	var out bytes.Buffer
	_, err := printAnnotations(printArgs(t, "--print-annotations", "--exclude", "a.go"), root, &out, &bytes.Buffer{})
	require.NoError(t, err)
	assert.Equal(t, "## a.go:4 (+)\nnew\n", out.String(), "a narrowed diff must not mark the session's annotations outdated")
}

func TestPrintAnnotations_RequiresGit(t *testing.T) {
	t.Chdir(t.TempDir())
	_, err := printAnnotations(printArgs(t, "--print-annotations"), t.TempDir(), &bytes.Buffer{}, &bytes.Buffer{})
	require.Error(t, err)
}
