package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"slices"
	"strings"
	"time"

	"github.com/jessevdk/go-flags"

	"github.com/umputun/revdiff/app/diff"
	"github.com/umputun/revdiff/app/notes"
	"github.com/umputun/revdiff/app/session"
)

// exit codes of `revdiff inbox --wait` besides 0 (replies printed) and 1 (error).
const (
	exitInboxTimeout = 3   // nothing arrived before --timeout
	exitInboxClosed  = 4   // the revdiff showing the notes closed with nothing pending
	exitInterrupted  = 130 // SIGINT / SIGTERM while waiting
)

// agentCommands are the first arguments that select the notes commands
// instead of a review. A branch with one of these names is reviewed with
// `revdiff -- notes` (or refs/heads/notes).
var agentCommands = []string{"notes", "note", "inbox"}

// isAgentCommand reports whether args start with a notes command.
func isAgentCommand(args []string) bool {
	return len(args) > 0 && slices.Contains(agentCommands, args[0])
}

// bodyArgs is a free-text positional argument; "-" reads it from stdin.
type bodyArgs struct {
	Body []string `positional-arg-name:"BODY" required:"1"`
}

// agentOptions are the notes commands Claude runs: import a walkthrough,
// add notes, answer the reviewer, and read the reviewer's replies.
type agentOptions struct {
	Ref    string `long:"ref" description:"reviewed ref, as passed to revdiff (default: uncommitted changes)"`
	Staged bool   `long:"staged" description:"notes of the staged-changes review"`

	Notes struct {
		Import struct {
			Args struct {
				File string `positional-arg-name:"FILE" required:"yes"`
			} `positional-args:"yes"`
		} `command:"import" description:"replace the branch's notes with a revdiff-notes/v1 file (- reads stdin)"`
	} `command:"notes" description:"Claude's notes on the review"`

	Note struct {
		Add struct {
			File    string   `long:"file" required:"yes" description:"file the note is on"`
			Line    int      `long:"line" required:"yes" description:"line number"`
			Side    string   `long:"side" default:"+" description:"side the line number counts in: + (new), - (removed), context"`
			EndLine int      `long:"end-line" description:"last line of a range"`
			Kind    string   `long:"kind" default:"explain" choice:"explain" choice:"caution" description:"note kind"`
			Args    bodyArgs `positional-args:"yes"`
		} `command:"add" description:"add a note on a line"`
		Overview struct {
			File string   `long:"file" required:"yes" description:"file the overview describes"`
			Args bodyArgs `positional-args:"yes"`
		} `command:"overview" description:"set a file's overview"`
		Reply struct {
			Args struct {
				ID   string   `positional-arg-name:"NOTE_ID" required:"yes"`
				Body []string `positional-arg-name:"BODY" required:"1"`
			} `positional-args:"yes"`
		} `command:"reply" description:"answer the discussion of a note"`
	} `command:"note" description:"add or answer one note"`

	Inbox struct {
		Wait    bool          `long:"wait" description:"block until a reply arrives"`
		Timeout time.Duration `long:"timeout" default:"10m" description:"how long --wait blocks"`
	} `command:"inbox" description:"print the reviewer's new replies as JSON lines"`
}

// agentEnv is everything the notes commands touch outside their arguments.
type agentEnv struct {
	ctx       context.Context
	stdin     io.Reader
	stdout    io.Writer
	stderr    io.Writer
	root      string        // review-sessions root
	pollEvery time.Duration // inbox --wait poll interval
	beatEvery time.Duration // inbox --wait listener heartbeat interval
}

// agentRun is one parsed notes command bound to its branch.
type agentRun struct {
	opts  agentOptions
	env   agentEnv
	store *notes.Store
	snap  *diffSnapshot
}

// runAgentCommand runs a notes command and returns the process exit code.
func runAgentCommand(args []string, env agentEnv) int {
	var opts agentOptions
	p := flags.NewParser(&opts, flags.HelpFlag|flags.PassDoubleDash)
	p.Name = "revdiff"
	if _, err := p.ParseArgs(args); err != nil {
		if fe, ok := errors.AsType[*flags.Error](err); ok && fe.Type == flags.ErrHelp {
			_, _ = fmt.Fprintln(env.stdout, err)
			return 0
		}
		_, _ = fmt.Fprintf(env.stderr, "error: %v\n", err)
		return 1
	}
	r := &agentRun{opts: opts, env: env}
	code, err := r.dispatch(p.Active)
	if err != nil {
		_, _ = fmt.Fprintf(env.stderr, "error: %v\n", err)
		return 1
	}
	return code
}

// dispatch runs the active (leaf) command.
func (r *agentRun) dispatch(active *flags.Command) (int, error) {
	if r.opts.Staged && strings.Contains(r.opts.Ref, "..") {
		return 0, errors.New("--staged cannot be used with a range --ref")
	}
	name := active.Name
	if active.Active != nil {
		name += " " + active.Active.Name
	}
	switch name {
	case "notes import":
		return 0, r.importNotes()
	case "note add":
		return 0, r.addNote()
	case "note overview":
		return 0, r.setOverview()
	case "note reply":
		return 0, r.reply()
	case "inbox":
		return r.inbox()
	default:
		return 0, fmt.Errorf("unknown command %q", name)
	}
}

// open resolves the review's branch and diff for ref. Notes need a git
// repository: they are kept in the branch's review-session directory.
func (r *agentRun) open(ref string) error {
	var vopts options
	vopts.Refs.Base = ref
	vopts.Staged = r.opts.Staged
	setup, err := setupVCSRenderer(vopts)
	if err != nil {
		return err
	}
	if setup.vcsType != diff.VCSGit {
		return errors.New("notes require a git repository (they are kept with the branch's review session)")
	}
	sessions, err := session.New(r.env.root, setup.gitRoot)
	if err != nil {
		return fmt.Errorf("review sessions unavailable: %w", err)
	}
	r.store = notes.New(sessions.BranchDir(sessions.BranchKey(ref)))
	r.snap = &diffSnapshot{
		renderer:           setup.renderer,
		ref:                ref,
		staged:             r.opts.Staged,
		untrackedFn:        setup.untrackedFn,
		untrackedRenamesFn: setup.untrackedRenamesFn,
		workDir:            setup.workDir,
		warnOut:            r.env.stderr,
		warnPrefix:         "notes",
	}
	return nil
}

// importNotes replaces the branch's notes with a notes file, anchoring every
// note against the current diff. The file's target ref is used when --ref is
// not given.
func (r *agentRun) importNotes() error {
	data, err := r.readInput(r.opts.Notes.Import.Args.File)
	if err != nil {
		return err
	}
	doc, err := notes.Parse(data)
	if err != nil {
		return fmt.Errorf("notes file: %w", err)
	}
	ref := r.opts.Ref
	if ref == "" {
		ref = doc.Target.Ref
	}
	doc.Target.Ref = ref
	if oerr := r.open(ref); oerr != nil {
		return oerr
	}
	known, err := r.snap.knownFiles()
	if err != nil {
		return err
	}
	for _, f := range doc.Files {
		lines, ok := r.fileLines(known, f.Path)
		if !ok {
			r.warnf("%s is not in the diff; its line notes are shown as outdated", f.Path)
		}
		doc.AnchorFile(f.Path, lines)
		r.warnOutdated(f.Path, doc.FileNotes(f.Path), ok)
	}
	for _, p := range doc.Tour {
		if doc.FileNotes(p) == nil {
			r.warnf("tour path %s has no notes and is skipped", p)
		}
	}
	saved, err := r.store.Update(func(cur *notes.Document) error {
		cur.Replace(doc)
		return nil
	})
	if err != nil {
		return fmt.Errorf("save notes: %w", err)
	}
	_, err = fmt.Fprintln(r.env.stdout, r.summary(saved))
	if err != nil {
		return fmt.Errorf("write summary: %w", err)
	}
	return nil
}

// summary describes an imported document in one line.
func (r *agentRun) summary(doc *notes.Document) string {
	total, overviews, outdated, files := 0, 0, 0, 0
	for _, f := range doc.Files {
		if len(f.Notes) > 0 {
			files++
		}
		for _, n := range f.Notes {
			total++
			switch {
			case n.Kind == notes.KindOverview:
				overviews++
			case n.Status == notes.StatusOutdated:
				outdated++
			}
		}
	}
	out := fmt.Sprintf("imported %d notes (%d overviews) on %d files", total, overviews, files)
	if outdated > 0 {
		out += fmt.Sprintf(", %d outdated", outdated)
	}
	if len(doc.Tour) > 0 {
		out += fmt.Sprintf(", tour of %d files", len(doc.TourOrder()))
	}
	return out
}

// addNote adds one line note, anchored against the current diff. A file that
// is not in the diff is rejected: the note could never be shown on a line.
func (r *agentRun) addNote() error {
	a := r.opts.Note.Add
	side, err := r.side(a.Side)
	if err != nil {
		return err
	}
	body, err := r.body(a.Args.Body)
	if err != nil {
		return err
	}
	if oerr := r.open(r.opts.Ref); oerr != nil {
		return oerr
	}
	known, err := r.snap.knownFiles()
	if err != nil {
		return err
	}
	lines, ok := r.fileLines(known, a.File)
	if !ok {
		return fmt.Errorf("%s is not in the diff", a.File)
	}
	var added notes.Note
	_, err = r.store.Update(func(doc *notes.Document) error {
		n, aerr := doc.AddNote(a.File, notes.Note{Kind: notes.Kind(a.Kind), Line: a.Line, Side: side, EndLine: a.EndLine, Body: body})
		if aerr != nil {
			return fmt.Errorf("add note: %w", aerr)
		}
		doc.AnchorFile(a.File, lines)
		found, _ := doc.Find(n.ID)
		added = *found
		return nil
	})
	if err != nil {
		return fmt.Errorf("save notes: %w", err)
	}
	if added.Status == notes.StatusOutdated {
		r.warnf("%s:%d (%s) is not in the diff; the note is shown as outdated", a.File, a.Line, a.Side)
	}
	return r.println(added.ID)
}

// setOverview sets a file's overview, keeping its discussion.
func (r *agentRun) setOverview() error {
	o := r.opts.Note.Overview
	body, err := r.body(o.Args.Body)
	if err != nil {
		return err
	}
	if oerr := r.open(r.opts.Ref); oerr != nil {
		return oerr
	}
	var id string
	_, err = r.store.Update(func(doc *notes.Document) error {
		n, aerr := doc.AddNote(o.File, notes.Note{Kind: notes.KindOverview, Body: body})
		if aerr != nil {
			return fmt.Errorf("set overview: %w", aerr)
		}
		id = n.ID
		return nil
	})
	if err != nil {
		return fmt.Errorf("save notes: %w", err)
	}
	return r.println(id)
}

// reply answers the discussion of a note.
func (r *agentRun) reply() error {
	a := r.opts.Note.Reply.Args
	body, err := r.body(a.Body)
	if err != nil {
		return err
	}
	if oerr := r.open(r.opts.Ref); oerr != nil {
		return oerr
	}
	_, err = r.store.Update(func(doc *notes.Document) error {
		if _, aerr := doc.Answer(a.ID, body, time.Now().UTC()); aerr != nil {
			return fmt.Errorf("reply: %w", aerr)
		}
		return nil
	})
	if err != nil {
		return fmt.Errorf("save notes: %w", err)
	}
	return r.println("replied to " + a.ID)
}

// inbox prints the reviewer's unconsumed replies, one JSON object per line.
// With --wait it blocks until at least one arrives (exit 0), --timeout passes
// (exitInboxTimeout) or the revdiff showing the notes closes
// (exitInboxClosed), keeping a listener marker fresh meanwhile so the TUI can
// tell an agent is listening.
func (r *agentRun) inbox() (int, error) {
	if oerr := r.open(r.opts.Ref); oerr != nil {
		return 0, oerr
	}
	items, err := r.store.Inbox()
	if err != nil {
		return 0, fmt.Errorf("read inbox: %w", err)
	}
	if len(items) > 0 || !r.opts.Inbox.Wait {
		return 0, r.printItems(items)
	}

	if merr := r.store.MarkListening(); merr != nil {
		return 0, fmt.Errorf("mark listening: %w", merr)
	}
	defer r.store.UnmarkListening()
	deadline := time.NewTimer(r.opts.Inbox.Timeout)
	defer deadline.Stop()
	poll := time.NewTicker(r.env.pollEvery)
	defer poll.Stop()
	lastBeat := time.Now()
	sawViewer := r.store.ViewerAlive()
	for {
		select {
		case <-r.env.ctx.Done():
			return exitInterrupted, nil
		case <-deadline.C:
			_, _ = fmt.Fprintf(r.env.stderr, "no replies within %s\n", r.opts.Inbox.Timeout)
			return exitInboxTimeout, nil
		case <-poll.C:
		}
		if items, err = r.store.Inbox(); err != nil {
			return 0, fmt.Errorf("read inbox: %w", err)
		}
		if len(items) > 0 {
			return 0, r.printItems(items)
		}
		viewing := r.store.ViewerAlive()
		if sawViewer && !viewing {
			_, _ = fmt.Fprintln(r.env.stderr, "the review closed")
			return exitInboxClosed, nil
		}
		sawViewer = sawViewer || viewing
		if time.Since(lastBeat) >= r.env.beatEvery {
			if merr := r.store.MarkListening(); merr != nil {
				return 0, fmt.Errorf("mark listening: %w", merr)
			}
			lastBeat = time.Now()
		}
	}
}

func (r *agentRun) printItems(items []notes.InboxItem) error {
	enc := json.NewEncoder(r.env.stdout)
	enc.SetEscapeHTML(false)
	for _, item := range items {
		if err := enc.Encode(item); err != nil {
			return fmt.Errorf("write inbox: %w", err)
		}
	}
	return nil
}

// fileLines returns the current diff lines of path, false when the file is
// not in the diff. A diff that cannot be read is warned about and treated as
// absent.
func (r *agentRun) fileLines(known map[string]diff.FileStatus, path string) ([]diff.DiffLine, bool) {
	status, ok := known[path]
	if !ok {
		return nil, false
	}
	lines, err := r.snap.fileLines(path, status)
	if err != nil {
		r.warnf("%v", err)
		return nil, false
	}
	return lines, true
}

// warnOutdated names every line note of path that could not be placed.
func (r *agentRun) warnOutdated(path string, ns []notes.Note, inDiff bool) {
	if !inDiff {
		return
	}
	for _, n := range ns {
		if n.Kind != notes.KindOverview && n.Status == notes.StatusOutdated {
			r.warnf("%s:%d (%s) is not in the diff; note %s is shown as outdated", path, n.Line, n.Side, n.ID)
		}
	}
}

// side maps the --side value to a note side.
func (r *agentRun) side(v string) (string, error) {
	switch v {
	case notes.SideNew, notes.SideOld:
		return v, nil
	case notes.SideContext, "context":
		return notes.SideContext, nil
	default:
		return "", fmt.Errorf("--side must be +, - or context, got %q", v)
	}
}

// body joins a free-text argument; a lone "-" reads it from stdin.
func (r *agentRun) body(parts []string) (string, error) {
	if len(parts) == 1 && parts[0] == "-" {
		data, err := io.ReadAll(r.env.stdin)
		if err != nil {
			return "", fmt.Errorf("read stdin: %w", err)
		}
		return string(data), nil
	}
	return strings.Join(parts, " "), nil
}

// readInput reads a file argument; "-" reads stdin.
func (r *agentRun) readInput(path string) ([]byte, error) {
	if path == "-" {
		data, err := io.ReadAll(r.env.stdin)
		if err != nil {
			return nil, fmt.Errorf("read stdin: %w", err)
		}
		return data, nil
	}
	data, err := os.ReadFile(path) //nolint:gosec // the agent names the file to import
	if err != nil {
		return nil, fmt.Errorf("read notes file: %w", err)
	}
	return data, nil
}

func (r *agentRun) println(s string) error {
	if _, err := fmt.Fprintln(r.env.stdout, s); err != nil {
		return fmt.Errorf("write output: %w", err)
	}
	return nil
}

func (r *agentRun) warnf(format string, args ...any) {
	_, _ = fmt.Fprintf(r.env.stderr, "warning: notes: "+format+"\n", args...)
}
