// Package notes keeps Claude's notes on a review: a short overview per file
// and explanations or cautions on specific lines, each with a discussion
// thread, plus an optional reading order (the tour). Notes are written by the
// agent through revdiff's notes commands, shown beside the diff by the TUI, and
// answered live: the reviewer's replies go to an append-only outbox the agent
// reads with `revdiff inbox`.
//
// Notes are a separate entity from the reviewer's annotations (change
// requests) and never appear in the annotation output. They are never stored
// in the repository: a branch's notes live in its review-session directory
// (see session.Store.BranchDir), shared by every session of that branch, in a
// notes/ subdirectory so the session listing (every *.json of the branch
// directory) never mistakes them for a session:
//
//	<branch dir>/notes/notes.json    the notes document (revdiff-notes/v1)
//	<branch dir>/notes/outbox.jsonl  reviewer replies, one JSON object per line
//	<branch dir>/notes/inbox.offset  how much of the outbox the agent has consumed
//	<branch dir>/notes/notes.lock    advisory lock serializing every writer
//	<branch dir>/notes/listener.pid  present while `revdiff inbox --wait` runs
//	<branch dir>/notes/viewer.pid    present while a revdiff TUI shows the notes
package notes

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/umputun/revdiff/app/annotation"
	"github.com/umputun/revdiff/app/diff"
)

// Format is the version tag every notes document carries.
const Format = "revdiff-notes/v1"

// Kind classifies a note.
type Kind string

// note kinds. An overview is file-level (one per file); explain and caution
// are attached to a line.
const (
	KindOverview Kind = "overview"
	KindExplain  Kind = "explain"
	KindCaution  Kind = "caution"
)

// Note sides: which version of the file Line counts in.
const (
	SideNew     = "+" // added line, or a context line by its new-side number
	SideOld     = "-" // removed line, by its old-side number
	SideContext = " " // context line, by its new-side number
)

// Status values. The zero value is current.
const (
	StatusCurrent  = ""
	StatusOutdated = "outdated"
)

// Thread authors.
const (
	AuthorYou    = "you"
	AuthorClaude = "claude"
)

// Reply states of a reviewer turn. An agent turn has no state.
const (
	TurnPending  = "pending"  // written by the reviewer, not yet read by the agent
	TurnRead     = "read"     // handed to the agent by `revdiff inbox`
	TurnAnswered = "answered" // the agent replied on the note since
)

// Document is a notes file: what was reviewed, the reading order and the
// notes of every file.
type Document struct {
	Format  string    `json:"format"`
	Target  Target    `json:"target,omitzero"`
	Author  string    `json:"author,omitempty"`
	Tour    []string  `json:"tour,omitempty"`
	Files   []File    `json:"files"`
	Updated time.Time `json:"updated,omitzero"`
}

// Target names the reviewed diff.
type Target struct {
	Ref  string `json:"ref,omitempty"`
	Head string `json:"head,omitempty"`
}

// File holds the notes of one path. Overview is an import shorthand for a
// note of kind overview; a parsed document carries the overview as a note.
type File struct {
	Path     string `json:"path"`
	Overview string `json:"overview,omitempty"`
	Notes    []Note `json:"notes,omitempty"`
}

// Note is one of Claude's notes.
type Note struct {
	ID      string             `json:"id,omitempty"`
	Kind    Kind               `json:"kind"`
	Line    int                `json:"line,omitempty"`
	Side    string             `json:"side,omitempty"`
	EndLine int                `json:"end_line,omitempty"`
	Anchor  *annotation.Anchor `json:"anchor,omitempty"`
	Body    string             `json:"body"`
	Status  string             `json:"status,omitempty"`
	Thread  []Turn             `json:"thread,omitempty"`
}

// Turn is one message of a note's discussion thread.
type Turn struct {
	ID     string    `json:"id"`
	Author string    `json:"author"`
	Body   string    `json:"body"`
	At     time.Time `json:"at"`
	State  string    `json:"state,omitempty"`
}

// Parse decodes and validates a notes document. Unknown fields are rejected
// so a typo in an agent-written file is reported instead of silently dropped.
// The result is normalized: a file's overview shorthand becomes an overview
// note at the head of its notes, a missing kind is explain, a missing side is
// "+", and every note without an id gets one.
func Parse(data []byte) (*Document, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var doc Document
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("decode notes: %w", err)
	}
	if dec.More() {
		return nil, errors.New("decode notes: trailing data after the document")
	}
	if err := doc.normalize(); err != nil {
		return nil, err
	}
	return &doc, nil
}

// normalize validates the document in place and fills defaults.
func (d *Document) normalize() error {
	if d.Format != Format {
		return fmt.Errorf("notes: format must be %q, got %q", Format, d.Format)
	}
	seenPath := make(map[string]bool, len(d.Files))
	seenID := map[string]bool{}
	for fi := range d.Files {
		f := &d.Files[fi]
		f.Path = strings.TrimSpace(f.Path)
		if f.Path == "" {
			return fmt.Errorf("notes: file %d has no path", fi+1)
		}
		if seenPath[f.Path] {
			return fmt.Errorf("notes: file %q is listed twice", f.Path)
		}
		seenPath[f.Path] = true
		if strings.TrimSpace(f.Overview) != "" {
			f.Notes = append([]Note{{Kind: KindOverview, Body: f.Overview}}, f.Notes...)
		}
		f.Overview = ""
		overviews := 0
		for ni := range f.Notes {
			n := &f.Notes[ni]
			if err := n.normalize(); err != nil {
				return fmt.Errorf("notes: %s note %d: %w", f.Path, ni+1, err)
			}
			if n.Kind == KindOverview {
				overviews++
			}
			if n.ID != "" {
				if seenID[n.ID] {
					return fmt.Errorf("notes: duplicate note id %q", n.ID)
				}
				seenID[n.ID] = true
			}
		}
		if overviews > 1 {
			return fmt.Errorf("notes: %s has %d overviews, at most one is allowed", f.Path, overviews)
		}
	}
	tour := make([]string, 0, len(d.Tour))
	for _, p := range d.Tour {
		p = strings.TrimSpace(p)
		if p == "" {
			return errors.New("notes: tour has an empty path")
		}
		if !slices.Contains(tour, p) {
			tour = append(tour, p)
		}
	}
	d.Tour = tour
	d.assignIDs()
	return nil
}

// normalize validates one note and fills its defaults.
func (n *Note) normalize() error {
	n.ID = strings.TrimSpace(n.ID)
	if n.Kind == "" {
		n.Kind = KindExplain
	}
	if strings.TrimSpace(n.Body) == "" {
		return errors.New("body is empty")
	}
	switch n.Kind {
	case KindOverview:
		n.Line, n.Side, n.EndLine, n.Anchor = 0, "", 0, nil
		return nil
	case KindExplain, KindCaution:
	default:
		return fmt.Errorf("unknown kind %q (want overview, explain or caution)", n.Kind)
	}
	if n.Line <= 0 {
		return fmt.Errorf("line must be positive, got %d", n.Line)
	}
	if n.Side == "" {
		n.Side = SideNew
	}
	if n.Side != SideNew && n.Side != SideOld && n.Side != SideContext {
		return fmt.Errorf("side must be \"+\", \"-\" or \" \", got %q", n.Side)
	}
	if n.EndLine != 0 && n.EndLine < n.Line {
		return fmt.Errorf("end_line %d is before line %d", n.EndLine, n.Line)
	}
	if n.Status != StatusCurrent && n.Status != StatusOutdated {
		return fmt.Errorf("unknown status %q", n.Status)
	}
	return nil
}

// assignIDs gives every note without an id the next free "n<k>".
func (d *Document) assignIDs() {
	used := map[string]bool{}
	for _, f := range d.Files {
		for _, n := range f.Notes {
			if n.ID != "" {
				used[n.ID] = true
			}
		}
	}
	next := 1
	for fi := range d.Files {
		for ni := range d.Files[fi].Notes {
			n := &d.Files[fi].Notes[ni]
			if n.ID != "" {
				continue
			}
			for used["n"+strconv.Itoa(next)] {
				next++
			}
			n.ID = "n" + strconv.Itoa(next)
			used[n.ID] = true
		}
	}
}

// Find returns the note with id and the path of its file, or nil.
func (d *Document) Find(id string) (*Note, string) {
	if d == nil {
		return nil, ""
	}
	for fi := range d.Files {
		for ni := range d.Files[fi].Notes {
			if d.Files[fi].Notes[ni].ID == id {
				return &d.Files[fi].Notes[ni], d.Files[fi].Path
			}
		}
	}
	return nil, ""
}

// file returns the entry of path, creating it when create is set.
func (d *Document) file(path string, create bool) *File {
	for i := range d.Files {
		if d.Files[i].Path == path {
			return &d.Files[i]
		}
	}
	if !create {
		return nil
	}
	d.Files = append(d.Files, File{Path: path})
	return &d.Files[len(d.Files)-1]
}

// overviewNote returns the overview note of the file, or nil.
func (f File) overviewNote() *Note {
	for i := range f.Notes {
		if f.Notes[i].Kind == KindOverview {
			return &f.Notes[i]
		}
	}
	return nil
}

// Count returns the number of notes in the document.
func (d *Document) Count() int {
	if d == nil {
		return 0
	}
	n := 0
	for _, f := range d.Files {
		n += len(f.Notes)
	}
	return n
}

// PendingReplies counts reviewer turns the agent has not answered yet.
func (d *Document) PendingReplies() int {
	if d == nil {
		return 0
	}
	n := 0
	for _, f := range d.Files {
		for _, note := range f.Notes {
			for _, t := range note.Thread {
				if t.Author == AuthorYou && t.State != TurnAnswered {
					n++
				}
			}
		}
	}
	return n
}

// Locate finds the diff line a line note belongs to in lines, returning its
// index or -1. A note with an anchor is found the way annotations are
// re-anchored (same text and change type, best context, nearest line). When
// the anchor is not found, or the note has none, the note's own line and side
// are matched, and accepted only when the anchor (if any) recorded that very
// text — so a note never silently lands on different code. Overview notes
// return -1.
func (n Note) Locate(lines []diff.DiffLine) int {
	if n.Kind == KindOverview || n.Line <= 0 {
		return -1
	}
	if n.Anchor != nil {
		if idx := n.Anchor.Locate(lines); idx >= 0 {
			return idx
		}
	}
	idx := n.lineIndex(lines)
	if idx < 0 {
		return -1
	}
	if n.Anchor != nil && lines[idx].Content != n.Anchor.Content {
		return -1
	}
	return idx
}

// lineIndex returns the index of the diff line at the note's line and side.
// A "+" note matches an added or a context line by its new-side number, "-" a
// removed line by its old-side number, " " a context line.
func (n Note) lineIndex(lines []diff.DiffLine) int {
	for i, dl := range lines {
		switch {
		case n.Side == SideOld && dl.ChangeType == diff.ChangeRemove && dl.OldNum == n.Line:
			return i
		case n.Side == SideContext && dl.ChangeType == diff.ChangeContext && dl.NewNum == n.Line:
			return i
		case n.Side == SideNew && (dl.ChangeType == diff.ChangeAdd || dl.ChangeType == diff.ChangeContext) && dl.NewNum == n.Line:
			return i
		}
	}
	return -1
}

// anchorTo places a line note on lines: found, it moves to the line it was
// found at, takes a fresh anchor and is current; not found, it keeps its
// position and becomes outdated. Reports whether the note was found.
func (n *Note) anchorTo(lines []diff.DiffLine) bool {
	if n.Kind == KindOverview {
		return true
	}
	idx := n.Locate(lines)
	if idx < 0 {
		n.Status = StatusOutdated
		return false
	}
	dl := lines[idx]
	newLine := dl.NewNum
	side := SideNew
	if dl.ChangeType == diff.ChangeRemove {
		newLine, side = dl.OldNum, SideOld
	}
	if n.EndLine > 0 {
		n.EndLine += newLine - n.Line
	}
	if n.Side == SideContext && dl.ChangeType == diff.ChangeContext {
		side = SideContext
	}
	n.Line, n.Side = newLine, side
	n.Anchor = annotation.NewAnchor(lines, idx)
	n.Status = StatusCurrent
	return true
}

// AnchorFile anchors every line note of path against the file's current diff
// lines (see anchorTo) and returns how many could not be placed. A nil lines
// slice means the file is not in the diff: every line note becomes outdated.
func (d *Document) AnchorFile(path string, lines []diff.DiffLine) int {
	f := d.file(path, false)
	if f == nil {
		return 0
	}
	outdated := 0
	for i := range f.Notes {
		n := &f.Notes[i]
		if n.Kind == KindOverview {
			continue
		}
		if lines == nil {
			n.Status = StatusOutdated
			outdated++
			continue
		}
		if !n.anchorTo(lines) {
			outdated++
		}
	}
	return outdated
}

// TourOrder returns the files that have notes in reading order: the tour's
// paths first, in tour order, then every other file with notes in document
// order. Tour paths without notes are skipped.
func (d *Document) TourOrder() []string {
	if d == nil {
		return nil
	}
	has := make(map[string]bool, len(d.Files))
	for _, f := range d.Files {
		if len(f.Notes) > 0 {
			has[f.Path] = true
		}
	}
	out := make([]string, 0, len(has))
	for _, p := range d.Tour {
		if has[p] && !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	for _, f := range d.Files {
		if has[f.Path] && !slices.Contains(out, f.Path) {
			out = append(out, f.Path)
		}
	}
	return out
}

// FileNotes returns the notes of path, or nil.
func (d *Document) FileNotes(path string) []Note {
	if d == nil {
		return nil
	}
	if f := d.file(path, false); f != nil {
		return f.Notes
	}
	return nil
}

// Replace makes incoming the document's content, keeping the discussion of
// every note whose id survives: a note in incoming without a thread takes the
// thread of the current note with the same id. incoming must be parsed.
func (d *Document) Replace(incoming *Document) {
	threads := map[string][]Turn{}
	for _, f := range d.Files {
		for _, n := range f.Notes {
			if len(n.Thread) > 0 {
				threads[n.ID] = n.Thread
			}
		}
	}
	*d = *incoming
	for fi := range d.Files {
		for ni := range d.Files[fi].Notes {
			n := &d.Files[fi].Notes[ni]
			if len(n.Thread) == 0 {
				n.Thread = threads[n.ID]
			}
		}
	}
}

// AddNote adds a note to path and returns it with its assigned id. An
// overview replaces the file's overview text, keeping its id and thread.
func (d *Document) AddNote(path string, n Note) (Note, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return Note{}, errors.New("note file is empty")
	}
	n.ID, n.Thread, n.Status = "", nil, StatusCurrent
	if err := n.normalize(); err != nil {
		return Note{}, err
	}
	f := d.file(path, true)
	if n.Kind == KindOverview {
		if cur := f.overviewNote(); cur != nil {
			cur.Body = n.Body
			return *cur, nil
		}
		f.Notes = append([]Note{n}, f.Notes...)
		d.assignIDs()
		return f.Notes[0], nil
	}
	f.Notes = append(f.Notes, n)
	d.assignIDs()
	return f.Notes[len(f.Notes)-1], nil
}

// Answer appends the agent's reply to note id and marks every earlier
// reviewer turn of that note answered.
func (d *Document) Answer(id, body string, at time.Time) (Turn, error) {
	body = strings.TrimSpace(body)
	if body == "" {
		return Turn{}, errors.New("reply is empty")
	}
	note, _ := d.Find(id)
	if note == nil {
		return Turn{}, fmt.Errorf("note %q not found", id)
	}
	for i := range note.Thread {
		if note.Thread[i].Author == AuthorYou {
			note.Thread[i].State = TurnAnswered
		}
	}
	return note.addTurn(AuthorClaude, body, at), nil
}
