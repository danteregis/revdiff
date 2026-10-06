package main

import (
	"fmt"
	"io"
	"path/filepath"

	"github.com/umputun/revdiff/app/diff"
	"github.com/umputun/revdiff/app/ui"
)

// diffSnapshot resolves a review's file set and per-file full-context diffs
// without the TUI. It backs the --annotations preload (which drops records
// whose lines are not in the diff) and --print-annotations (which re-anchors a
// review session's annotations against the current diff), so both see the
// same files and the same rename-aware diffs the UI shows.
type diffSnapshot struct {
	renderer           ui.Renderer
	ref                string
	staged             bool
	untrackedFn        func() ([]string, error)
	untrackedRenamesFn func([]string) ([]diff.FileEntry, error)
	workDir            string
	warnOut            io.Writer
	warnPrefix         string // flag named in warnings, e.g. "--annotations"

	// renames maps a file's current path to its rename origin so the
	// per-file diff is rename-aware (matches the displayed minimal diff).
	renames map[string]string
}

// knownFiles returns the review's files with their status. Mirrors
// ui.loadFiles' assembly of the visible file set with one deliberate
// divergence: untracked files are always folded in here, whereas the UI only
// surfaces them when its show-untracked toggle is on. Both callers run
// upstream of (or without) the toggle, so they accept either viewing mode.
//
// When running with no ref and no --staged, staged-only FileAdded entries
// are folded in if the unstaged set is empty (matches the UI's empty-diff
// fallback).
func (s *diffSnapshot) knownFiles() (map[string]diff.FileStatus, error) {
	if s.renames == nil {
		s.renames = make(map[string]string)
	}
	files, err := s.renderer.ChangedFiles(s.ref, s.staged)
	if err != nil {
		return nil, fmt.Errorf("resolve diff: %w", err)
	}
	known := make(map[string]diff.FileStatus, len(files))
	for _, fe := range files {
		known[fe.Path] = fe.Status
		if fe.OldPath != "" {
			s.renames[fe.Path] = fe.OldPath
		}
	}
	if s.untrackedFn != nil {
		if ut, utErr := s.untrackedFn(); utErr != nil {
			s.warnf("list untracked files: %v", utErr)
		} else {
			for _, path := range ut {
				if _, ok := known[path]; !ok {
					known[path] = diff.FileUntracked
				}
			}
			s.foldUntrackedRenames(ut, known)
		}
	}
	if s.ref != "" || s.staged || len(files) > 0 {
		return known, nil
	}
	stagedFiles, sErr := s.renderer.ChangedFiles("", true)
	if sErr != nil {
		s.warnf("resolve staged files: %v", sErr)
		return known, nil
	}
	for _, fe := range stagedFiles {
		if _, ok := known[fe.Path]; ok {
			continue
		}
		if fe.Status == diff.FileAdded {
			known[fe.Path] = fe.Status
		}
	}
	return known, nil
}

// foldUntrackedRenames upgrades untracked files that are actually working-tree
// renames (plain `mv old new`, new still untracked) to FileRenamed, records the
// rename origin in s.renames, and drops the standalone deletion of the origin.
// This mirrors ui.detectUntrackedRenames + mergeUntrackedEntries so the per-file
// diff matches the displayed rename-aware diff. No-op unless a git detector is
// wired and the review is in unstaged working-tree mode (the only mode where new
// stays untracked).
func (s *diffSnapshot) foldUntrackedRenames(untracked []string, known map[string]diff.FileStatus) {
	if s.untrackedRenamesFn == nil || s.ref != "" || s.staged {
		return
	}
	renames, err := s.untrackedRenamesFn(untracked)
	if err != nil {
		s.warnf("detect untracked renames: %v", err)
		return
	}
	for _, r := range renames {
		known[r.Path] = diff.FileRenamed
		s.renames[r.Path] = r.OldPath
		delete(known, r.OldPath)
	}
}

// fileLines returns the full-context diff of file. Mirrors
// ui.fetchEffectiveFileDiff: staged-only FileAdded entries use --cached when
// the request was unstaged, and FileUntracked entries are read from disk as
// all-added lines. Renamed files carry OldPath so the diff is rename-aware.
// knownFiles must run first so rename origins are recorded.
func (s *diffSnapshot) fileLines(file string, status diff.FileStatus) ([]diff.DiffLine, error) {
	if status == diff.FileUntracked && s.workDir != "" {
		lines, err := diff.ReadFileAsAdded(filepath.Join(s.workDir, file))
		if err != nil {
			return nil, fmt.Errorf("read %q: %w", file, err)
		}
		return lines, nil
	}
	fileStaged := s.staged
	if !s.staged && s.ref == "" && status == diff.FileAdded {
		fileStaged = true
	}
	lines, err := s.renderer.FileDiff(diff.FileDiffRequest{Ref: s.ref, Path: file, OldPath: s.renames[file], Staged: fileStaged})
	if err != nil {
		return nil, fmt.Errorf("diff for %q: %w", file, err)
	}
	return lines, nil
}

func (s *diffSnapshot) warnf(format string, args ...any) {
	if s.warnOut == nil {
		return
	}
	_, _ = fmt.Fprintf(s.warnOut, "warning: "+s.warnPrefix+": "+format+"\n", args...)
}
