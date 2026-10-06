package main

import (
	"errors"
	"fmt"
	"io"

	"github.com/umputun/revdiff/app/annotation"
	"github.com/umputun/revdiff/app/diff"
	"github.com/umputun/revdiff/app/session"
)

// --print-annotations modes.
const (
	printPending = "pending" // annotations not yet handed to the agent
	printAll     = "all"     // every open annotation, delivered or not
)

// validatePrintAnnotationsFlag rejects --print-annotations combined with a
// mode that has no review session (stdin, compare, all-files, standalone
// files), with sessions turned off, with a request for a fresh session, or
// with another print-and-exit command.
func validatePrintAnnotationsFlag(opts options) error {
	if opts.PrintAnnotations == "" {
		return nil
	}
	conflicts := []struct {
		bad  bool
		flag string
	}{
		{opts.Stdin, "--stdin"},
		{opts.CompareOld != "" || opts.CompareNew != "", "--compare-old/--compare-new"},
		{opts.AllFiles, "--all-files"},
		{len(opts.Only) > 0, "--only"},
		{opts.Annotations != "", "--annotations"},
		{opts.NoSession, "--no-session"},
		{opts.Session == "new", "--session=new"},
		{opts.DumpConfig, "--dump-config"},
		{opts.DumpKeys, "--dump-keys"},
		{opts.DumpTheme, "--dump-theme"},
		{opts.ListThemes, "--list-themes"},
		{opts.InitThemes, "--init-themes"},
		{opts.InitAllThemes, "--init-all-themes"},
		{len(opts.InstallTheme) > 0, "--install-theme"},
	}
	for _, c := range conflicts {
		if c.bad {
			return fmt.Errorf("--print-annotations cannot be used with %s", c.flag)
		}
	}
	return nil
}

// printSessionStore is the part of session.Store the print command uses.
type printSessionStore interface {
	Open(req session.Request) (session.Opened, error)
	Save(s *session.Session) error
}

// annotationPrinter prints a review session's annotations without starting
// the TUI. It opens the session exactly as a launch with the same refs and
// --session would, re-anchors its annotations against the current diff (as
// the UI does on every file-list load, so an annotation whose code changed is
// outdated and not printed), writes them in the FormatOutput format and marks
// the printed pending ones delivered.
type annotationPrinter struct {
	opts     options
	sessions printSessionStore
	snap     *diffSnapshot
	stdout   io.Writer
	stderr   io.Writer
}

// printAnnotations runs --print-annotations in the current directory's git
// repository, keeping sessions under root.
func printAnnotations(opts options, root string, stdout, stderr io.Writer) (int, error) {
	// the session covers the whole branch, so the printed set is never narrowed
	// by --include / --exclude (which may come from the config file)
	opts.Include, opts.Exclude = nil, nil
	setup, err := setupVCSRenderer(opts)
	if err != nil {
		return 0, err
	}
	if setup.vcsType != diff.VCSGit {
		return 0, errors.New("--print-annotations requires a git repository (review sessions are git-only)")
	}
	store, err := session.New(root, setup.gitRoot)
	if err != nil {
		return 0, fmt.Errorf("review sessions unavailable: %w", err)
	}
	p := annotationPrinter{
		opts:     opts,
		sessions: store,
		snap: &diffSnapshot{
			renderer:           setup.renderer,
			ref:                opts.ref(),
			staged:             opts.Staged,
			untrackedFn:        setup.untrackedFn,
			untrackedRenamesFn: setup.untrackedRenamesFn,
			workDir:            setup.workDir,
			warnOut:            stderr,
			warnPrefix:         "--print-annotations",
		},
		stdout: stdout,
		stderr: stderr,
	}
	return p.run()
}

// run prints the selected annotations. A pending print marks what it printed
// delivered; an "all" print re-sends every open annotation and marks the
// pending ones among them delivered too. The session is saved only when
// re-anchoring or delivery changed it, and only after the output was written.
func (p annotationPrinter) run() (int, error) {
	name, _ := p.opts.sessionStart()
	opened, err := p.sessions.Open(session.Request{Ref: p.opts.ref(), Staged: p.opts.Staged, Name: name})
	if err != nil {
		return 0, fmt.Errorf("open review session: %w", err)
	}
	sess := opened.Session
	if sess == nil || !opened.Resumed {
		if name != "" {
			return 0, fmt.Errorf("review session %q not found", name)
		}
		_, _ = fmt.Fprintln(p.stderr, "no review session for this branch")
		return p.write("")
	}

	store := annotation.NewStore()
	for _, a := range sess.Annotations {
		store.Add(a)
	}
	changed, err := p.reanchor(store)
	if err != nil {
		return 0, err
	}

	out := store.FormatOutput()
	if p.opts.PrintAnnotations == printAll {
		out = store.FormatOpen()
	}
	code, err := p.write(out)
	if err != nil {
		return 0, err
	}
	if store.MarkDelivered() > 0 {
		changed = true
	}
	if !changed {
		return code, nil
	}
	sess.Annotations = make([]annotation.Annotation, 0, store.Count())
	for _, file := range store.Files() {
		sess.Annotations = append(sess.Annotations, store.Get(file)...)
	}
	if err := p.sessions.Save(sess); err != nil {
		// the annotations were printed; failing now would read as "nothing sent"
		_, _ = fmt.Fprintf(p.stderr, "warning: --print-annotations: save review session: %v\n", err)
	}
	return code, nil
}

// reanchor re-anchors every annotated file still in the diff against its
// current full-context diff, and marks the annotations of a file that left
// the diff outdated (resolved ones stay resolved) — the rules the UI applies
// on load (ui.applyReanchored). A file whose diff cannot be read keeps its
// annotations as saved, with a warning. Reports whether anything changed.
func (p annotationPrinter) reanchor(store *annotation.Store) (bool, error) {
	known, err := p.snap.knownFiles()
	if err != nil {
		return false, fmt.Errorf("--print-annotations: %w", err)
	}
	changed := false
	for _, file := range store.Files() {
		current := store.Get(file)
		status, ok := known[file]
		if !ok {
			next := make([]annotation.Annotation, len(current))
			for i, a := range current {
				if a.Status != annotation.StatusResolved && a.Status != annotation.StatusOutdated {
					a.Status = annotation.StatusOutdated
					changed = true
				}
				next[i] = a
			}
			store.ReplaceFile(file, next)
			continue
		}
		lines, lerr := p.snap.fileLines(file, status)
		if lerr != nil {
			p.snap.warnf("re-anchor annotations: %v", lerr)
			continue
		}
		next, fileChanged := annotation.ReanchorFile(current, lines)
		if fileChanged {
			store.ReplaceFile(file, next)
			changed = true
		}
	}
	return changed, nil
}

// write sends out to -o (written even when empty, so a stale file never reads
// as fresh output) or stdout, with the interactive exit's exit code.
func (p annotationPrinter) write(out string) (int, error) {
	return writeAnnotationOutput(annotationOutputReq{opts: p.opts, output: out, stdout: p.stdout})
}
