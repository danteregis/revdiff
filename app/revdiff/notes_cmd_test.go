package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/umputun/revdiff/app/notes"
	"github.com/umputun/revdiff/app/session"
)

type agentTest struct {
	root   string
	repo   string
	stdout *bytes.Buffer
	stderr *bytes.Buffer
}

// newAgentTest creates printTestRepo's repository (a.go with func B added in
// the working tree) and an isolated sessions root.
func newAgentTest(t *testing.T) *agentTest {
	t.Helper()
	return &agentTest{root: t.TempDir(), repo: printTestRepo(t), stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{}}
}

func (a *agentTest) env(ctx context.Context, stdin string) agentEnv {
	return agentEnv{ctx: ctx, stdin: strings.NewReader(stdin), stdout: a.stdout, stderr: a.stderr, root: a.root,
		pollEvery: 10 * time.Millisecond, beatEvery: 20 * time.Millisecond}
}

func (a *agentTest) run(t *testing.T, args ...string) int {
	t.Helper()
	a.stdout.Reset()
	a.stderr.Reset()
	return runAgentCommand(args, a.env(context.Background(), ""))
}

// store opens the notes store of the checked-out branch the way the TUI does.
func (a *agentTest) store(t *testing.T) *notes.Store {
	t.Helper()
	s, err := session.New(a.root, a.repo)
	require.NoError(t, err)
	return notes.New(s.BranchDir(s.BranchKey("")))
}

func (a *agentTest) writeNotes(t *testing.T, doc string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "notes.json")
	require.NoError(t, os.WriteFile(path, []byte(doc), 0o600))
	return path
}

const agentTestDoc = `{"format":"revdiff-notes/v1","author":"claude","tour":["a.go","gone.go"],
"files":[{"path":"a.go","overview":"package a","notes":[
  {"kind":"caution","line":4,"body":"B is new"},
  {"line":40,"body":"no such line"}]},
 {"path":"gone.go","notes":[{"line":1,"body":"not in the diff"}]}]}`

func TestNotesUIStore(t *testing.T) {
	assert.Nil(t, notesUIStore(nil), "no repo is a true nil interface, not a typed nil")
	repo := notes.NewRepo(func(string) string { return t.TempDir() })
	assert.Same(t, repo, notesUIStore(repo))
}

func TestIsAgentCommand(t *testing.T) {
	assert.True(t, isAgentCommand([]string{"notes", "import", "x"}))
	assert.True(t, isAgentCommand([]string{"note"}))
	assert.True(t, isAgentCommand([]string{"inbox", "--wait"}))
	assert.False(t, isAgentCommand(nil))
	assert.False(t, isAgentCommand([]string{"--", "notes"}))
	assert.False(t, isAgentCommand([]string{"main", "notes"}))
}

func TestRunAgentCommand_ParseErrors(t *testing.T) {
	a := &agentTest{root: t.TempDir(), stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{}}
	tests := []struct {
		args []string
		want string
	}{
		{[]string{"notes"}, "specify the import command"},
		{[]string{"notes", "import"}, "FILE"},
		{[]string{"note", "add", "--line", "3", "x"}, "--file"},
		{[]string{"note", "add", "--file", "a.go", "--line", "3", "--kind", "todo", "x"}, "Invalid value"},
		{[]string{"note", "reply", "n1"}, "BODY"},
		{[]string{"inbox", "--timeout", "soon"}, "invalid"},
		{[]string{"inbox", "--staged", "--ref", "a..b"}, "--staged cannot be used with a range --ref"},
	}
	for _, tt := range tests {
		t.Run(strings.Join(tt.args, " "), func(t *testing.T) {
			assert.Equal(t, 1, a.run(t, tt.args...))
			assert.Contains(t, a.stderr.String(), tt.want)
		})
	}

	assert.Equal(t, 0, a.run(t, "note", "--help"))
	assert.Contains(t, a.stdout.String(), "reply")
}

func TestRunAgentCommand_NotGit(t *testing.T) {
	t.Chdir(t.TempDir())
	a := &agentTest{root: t.TempDir(), stdout: &bytes.Buffer{}, stderr: &bytes.Buffer{}}
	assert.Equal(t, 1, a.run(t, "inbox"))
	assert.Contains(t, a.stderr.String(), "error:")
}

func TestRunAgentCommand_Import(t *testing.T) {
	a := newAgentTest(t)
	path := a.writeNotes(t, agentTestDoc)
	require.Equal(t, 0, a.run(t, "notes", "import", path), a.stderr.String())
	assert.Equal(t, "imported 4 notes (1 overviews) on 2 files, 2 outdated, tour of 2 files\n", a.stdout.String())
	assert.Contains(t, a.stderr.String(), "a.go:40 (+) is not in the diff")
	assert.Contains(t, a.stderr.String(), "gone.go is not in the diff")

	doc, err := a.store(t).Load()
	require.NoError(t, err)
	require.NotNil(t, doc)
	ns := doc.FileNotes("a.go")
	require.Len(t, ns, 3)
	require.NotNil(t, ns[1].Anchor, "anchored against the current diff")
	assert.Equal(t, "func B() {}", ns[1].Anchor.Content)
	assert.Equal(t, notes.StatusCurrent, ns[1].Status)
	assert.Equal(t, notes.StatusOutdated, ns[2].Status)
	assert.Equal(t, notes.StatusOutdated, doc.FileNotes("gone.go")[0].Status)

	// a thread survives a re-import that keeps the note id
	_, err = a.store(t).Reply(ns[1].ID, "why?")
	require.NoError(t, err)
	again := a.writeNotes(t, `{"format":"revdiff-notes/v1","files":[{"path":"a.go","notes":[{"id":"`+ns[1].ID+`","line":4,"body":"reworded"}]}]}`)
	require.Equal(t, 0, a.run(t, "notes", "import", again), a.stderr.String())
	doc, err = a.store(t).Load()
	require.NoError(t, err)
	n, _ := doc.Find(ns[1].ID)
	require.NotNil(t, n)
	assert.Equal(t, "reworded", n.Body)
	assert.Len(t, n.Thread, 1)
	assert.Len(t, doc.FileNotes("a.go"), 1, "the import replaces the notes")
}

func TestRunAgentCommand_ImportStdinAndErrors(t *testing.T) {
	a := newAgentTest(t)
	code := runAgentCommand([]string{"notes", "import", "-"}, a.env(context.Background(), `{"format":"revdiff-notes/v1","files":[{"path":"a.go","overview":"o"}]}`))
	require.Equal(t, 0, code, a.stderr.String())
	assert.Contains(t, a.stdout.String(), "imported 1 notes (1 overviews) on 1 files")

	assert.Equal(t, 1, a.run(t, "notes", "import", filepath.Join(t.TempDir(), "missing.json")))
	assert.Contains(t, a.stderr.String(), "read notes file")
	assert.Equal(t, 1, a.run(t, "notes", "import", a.writeNotes(t, `{"format":"v0"}`)))
	assert.Contains(t, a.stderr.String(), "format must be")
}

func TestRunAgentCommand_NoteAddOverviewReply(t *testing.T) {
	a := newAgentTest(t)
	require.Equal(t, 0, a.run(t, "note", "add", "--file", "a.go", "--line", "4", "--kind", "caution", "B", "is", "new"), a.stderr.String())
	assert.Equal(t, "n1\n", a.stdout.String())

	require.Equal(t, 0, a.run(t, "note", "add", "--file", "a.go", "--line", "3", "--side", "context", "context", "line"), a.stderr.String())
	assert.Equal(t, "n2\n", a.stdout.String())

	require.Equal(t, 0, a.run(t, "note", "add", "--file", "a.go", "--line", "30", "far"), a.stderr.String())
	assert.Contains(t, a.stderr.String(), "is not in the diff; the note is shown as outdated")

	assert.Equal(t, 1, a.run(t, "note", "add", "--file", "nope.go", "--line", "1", "x"))
	assert.Contains(t, a.stderr.String(), "nope.go is not in the diff")
	assert.Equal(t, 1, a.run(t, "note", "add", "--file", "a.go", "--line", "1", "--side", "x", "y"))
	assert.Contains(t, a.stderr.String(), "--side must be")

	code := runAgentCommand([]string{"note", "overview", "--file", "a.go", "-"}, a.env(context.Background(), "multi\nline overview\n"))
	require.Equal(t, 0, code, a.stderr.String())

	doc, err := a.store(t).Load()
	require.NoError(t, err)
	ns := doc.FileNotes("a.go")
	require.Len(t, ns, 4)
	assert.Equal(t, notes.KindOverview, ns[0].Kind)
	assert.Equal(t, "multi\nline overview\n", ns[0].Body)
	assert.Equal(t, notes.KindCaution, ns[1].Kind)
	assert.Equal(t, "B is new", ns[1].Body)
	assert.Equal(t, notes.SideContext, ns[2].Side)
	assert.Equal(t, notes.StatusOutdated, ns[3].Status)

	_, err = a.store(t).Reply("n1", "is it?")
	require.NoError(t, err)
	require.Equal(t, 0, a.run(t, "note", "reply", "n1", "yes,", "it", "is"), a.stderr.String())
	assert.Equal(t, "replied to n1\n", a.stdout.String())
	doc, err = a.store(t).Load()
	require.NoError(t, err)
	n, _ := doc.Find("n1")
	require.Len(t, n.Thread, 2)
	assert.Equal(t, notes.TurnAnswered, n.Thread[0].State)
	assert.Equal(t, "yes, it is", n.Thread[1].Body)

	assert.Equal(t, 1, a.run(t, "note", "reply", "n99", "x"))
	assert.Contains(t, a.stderr.String(), `note "n99" not found`)
}

func TestRunAgentCommand_Inbox(t *testing.T) {
	a := newAgentTest(t)
	require.Equal(t, 0, a.run(t, "note", "add", "--file", "a.go", "--line", "4", "explain", "B"), a.stderr.String())

	require.Equal(t, 0, a.run(t, "inbox"))
	assert.Empty(t, a.stdout.String(), "nothing yet")

	_, err := a.store(t).Reply("n1", "what does B do?")
	require.NoError(t, err)
	require.Equal(t, 0, a.run(t, "inbox"))
	var item notes.InboxItem
	require.NoError(t, json.Unmarshal(a.stdout.Bytes(), &item))
	assert.Equal(t, "n1", item.Note.ID)
	assert.Equal(t, "a.go", item.Note.File)
	assert.Equal(t, 4, item.Note.Line)
	assert.Equal(t, "explain B", item.Note.Body)
	assert.False(t, item.Note.Outdated)
	assert.Equal(t, "what does B do?", item.Reply.Body)

	require.Equal(t, 0, a.run(t, "inbox"))
	assert.Empty(t, a.stdout.String(), "consumed")
}

func TestRunAgentCommand_InboxWait(t *testing.T) {
	a := newAgentTest(t)
	require.Equal(t, 0, a.run(t, "note", "add", "--file", "a.go", "--line", "4", "explain"), a.stderr.String())
	store := a.store(t)

	t.Run("timeout", func(t *testing.T) {
		start := time.Now()
		assert.Equal(t, exitInboxTimeout, a.run(t, "inbox", "--wait", "--timeout", "80ms"))
		assert.GreaterOrEqual(t, time.Since(start), 80*time.Millisecond)
		assert.Contains(t, a.stderr.String(), "no replies within 80ms")
		assert.False(t, store.ListenerAlive(), "the listener marker is removed on exit")
	})

	t.Run("reply arrives", func(t *testing.T) {
		done := make(chan struct{})
		go func() {
			defer close(done)
			assert.Eventually(t, store.ListenerAlive, time.Second, 5*time.Millisecond, "listening while waiting")
			_, err := store.Reply("n1", "late question")
			assert.NoError(t, err)
		}()
		assert.Equal(t, 0, a.run(t, "inbox", "--wait", "--timeout", "5s"))
		<-done
		assert.Contains(t, a.stdout.String(), "late question")
		assert.False(t, store.ListenerAlive())
	})

	t.Run("review closes", func(t *testing.T) {
		require.NoError(t, store.MarkViewing())
		go func() {
			time.Sleep(60 * time.Millisecond)
			store.UnmarkViewing()
		}()
		assert.Equal(t, exitInboxClosed, a.run(t, "inbox", "--wait", "--timeout", "5s"))
		assert.Contains(t, a.stderr.String(), "the review closed")
	})

	t.Run("interrupted", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			time.Sleep(30 * time.Millisecond)
			cancel()
		}()
		assert.Equal(t, exitInterrupted, runAgentCommand([]string{"inbox", "--wait"}, a.env(ctx, "")))
		assert.False(t, store.ListenerAlive())
	})
}
