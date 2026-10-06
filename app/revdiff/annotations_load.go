package main

import (
	"fmt"
	"io"
	"os"

	"github.com/umputun/revdiff/app/annotation"
	"github.com/umputun/revdiff/app/diff"
	"github.com/umputun/revdiff/app/ui"
)

// maxAnnotationsFileSize caps the bytes read from --annotations. Annotation
// files aggregate many records (LLM reviews, history exports) so 1 MiB is a
// generous practical ceiling; anything larger almost certainly indicates the
// flag was pointed at the wrong file.
const maxAnnotationsFileSize = 1 << 20

// preloader bundles the inputs and warning sink for --annotations preload.
// The file set and per-file diffs come from the shared diffSnapshot.
type preloader struct {
	store   *annotation.Store
	snap    *diffSnapshot
	warnOut io.Writer

	// lineCache memoises the (line, change-type) set per file so
	// repeated annotations on the same file do not re-fetch its diff.
	lineCache map[string]map[lineKey]struct{}
}

type lineKey struct {
	line int
	kind string
}

// preloadAnnotations parses the markdown file at path (same format as
// Store.FormatOutput) and feeds each record through store.Add after dropping
// orphans against the resolved diff. file-level records (Line == 0) require
// only that the file be present in ChangedFiles. Line-scoped records require
// both the file and a matching DiffLine (same line number for the record's
// change type) to be present. Untracked files (surfaced in the UI via the
// show-untracked toggle) are folded in via untrackedFn so annotations saved
// against them round-trip; their line-set is read from disk as all-added
// lines, mirroring ui.resolveEmptyDiff. Dropped records are warned to warnOut.
func preloadAnnotations(path string, store *annotation.Store, renderer ui.Renderer, ref string, staged bool,
	untrackedFn func() ([]string, error), untrackedRenamesFn func([]string) ([]diff.FileEntry, error),
	workDir string, warnOut io.Writer,
) error {
	records, err := readAnnotationsFile(path)
	if err != nil {
		return err
	}

	p := &preloader{
		store: store,
		snap: &diffSnapshot{
			renderer:           renderer,
			ref:                ref,
			staged:             staged,
			untrackedFn:        untrackedFn,
			untrackedRenamesFn: untrackedRenamesFn,
			workDir:            workDir,
			warnOut:            warnOut,
			warnPrefix:         "--annotations",
		},
		warnOut:   warnOut,
		lineCache: make(map[string]map[lineKey]struct{}),
	}
	return p.load(records)
}

// readAnnotationsFile rejects non-regular files (FIFO, device, anything that
// would block os.Open) and oversize inputs before parsing. The size guard
// runs first against info.Size() so we never start scanning a 100 MiB file
// just to reject it; io.LimitReader is layered on top as belt-and-braces in
// case the file grows between Stat and Open.
func readAnnotationsFile(path string) ([]annotation.Annotation, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("open annotations file: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("open annotations file: %s: not a regular file", path)
	}
	if info.Size() > maxAnnotationsFileSize {
		return nil, fmt.Errorf("annotations file %s exceeds %d bytes (got %d)", path, maxAnnotationsFileSize, info.Size())
	}
	f, err := os.Open(path) //nolint:gosec // user-supplied path is intentional
	if err != nil {
		return nil, fmt.Errorf("open annotations file: %w", err)
	}
	defer f.Close()

	records, err := annotation.Parse(io.LimitReader(f, maxAnnotationsFileSize+1))
	if err != nil {
		return nil, fmt.Errorf("parse annotations: %w", err)
	}
	return records, nil
}

func (p *preloader) load(records []annotation.Annotation) error {
	known, err := p.snap.knownFiles()
	if err != nil {
		return fmt.Errorf("annotation preload: %w", err)
	}

	for _, a := range records {
		// Sanitize the comment text before it reaches Store.Add: the
		// preload source is user- or LLM-supplied and may carry stray
		// ANSI / CR-overwrite / C1 bytes that Renderer.AnnotationInline
		// wraps into the TUI verbatim. Mirrors diff.SanitizeCommitText
		// usage on commit Author/Subject/Body.
		a.Comment = diff.SanitizeCommitText(a.Comment)

		status, ok := known[a.File]
		if !ok {
			p.warnf("warning: --annotations: file %q not in diff, dropping annotation\n", a.File)
			continue
		}
		if a.Line == 0 {
			p.store.Add(a)
			continue
		}
		lines := p.lookupLineSet(a.File, status)
		if _, ok := lines[lineKey{line: a.Line, kind: a.Type}]; !ok {
			p.warnf("warning: --annotations: %s:%d (%s) not in diff, dropping\n", a.File, a.Line, a.Type)
			continue
		}
		p.store.Add(a)
	}
	return nil
}

// lookupLineSet returns the cached line-set for file, fetching its diff
// through the snapshot on miss.
func (p *preloader) lookupLineSet(file string, status diff.FileStatus) map[lineKey]struct{} {
	if lines, ok := p.lineCache[file]; ok {
		return lines
	}
	lines := map[lineKey]struct{}{}
	if dl, err := p.snap.fileLines(file, status); err != nil {
		p.warnf("warning: --annotations: %v\n", err)
	} else {
		lines = buildLineSet(dl)
	}
	p.lineCache[file] = lines
	return lines
}

func (p *preloader) warnf(format string, args ...any) {
	_, _ = fmt.Fprintf(p.warnOut, format, args...)
}

// buildLineSet maps each renderable diff line to its (line-number, change-type)
// key. Mirrors Model.diffLineNum: removals key on OldNum, all other change
// types key on NewNum.
func buildLineSet(lines []diff.DiffLine) map[lineKey]struct{} {
	out := make(map[lineKey]struct{}, len(lines))
	for _, dl := range lines {
		if dl.ChangeType == diff.ChangeDivider {
			continue
		}
		var n int
		switch dl.ChangeType {
		case diff.ChangeRemove:
			n = dl.OldNum
		default:
			n = dl.NewNum
		}
		if n == 0 {
			continue
		}
		out[lineKey{line: n, kind: string(dl.ChangeType)}] = struct{}{}
	}
	return out
}
