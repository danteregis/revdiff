package notes

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/umputun/revdiff/app/fsutil"
)

// file names inside a branch directory.
const (
	notesFile  = "notes.json"
	outboxFile = "outbox.jsonl"
	offsetFile = "inbox.offset"
	lockFile   = "notes.lock"
)

// Store reads and writes the notes of one branch, kept in that branch's
// review-session directory. Every write (TUI or CLI) holds an exclusive
// advisory lock (flock) on notes.lock and replaces notes.json atomically, so
// concurrent writers serialize and a reader never sees a partial file; reads
// take no lock.
type Store struct {
	dir string
	now func() time.Time
}

// notesDir is the subdirectory of a branch's session directory holding its notes.
const notesDir = "notes"

// New returns the Store of the branch whose session directory is branchDir;
// the notes live in its notes/ subdirectory. Nothing is created until the
// first write.
func New(branchDir string) *Store {
	return &Store{dir: filepath.Join(branchDir, notesDir), now: time.Now}
}

// Stamp identifies one version of notes.json for change polling: a write
// replaces the file through a rename, so any write changes the inode even
// when size and modification time happen to match. The zero Stamp means no
// file.
type Stamp struct {
	ModTime int64
	Size    int64
	Inode   uint64
}

// Stamp returns the current version stamp of notes.json (zero when missing).
func (s *Store) Stamp() Stamp {
	fi, err := os.Stat(s.path(notesFile))
	if err != nil {
		return Stamp{}
	}
	st := Stamp{ModTime: fi.ModTime().UnixNano(), Size: fi.Size()}
	if sys, ok := fi.Sys().(*syscall.Stat_t); ok {
		st.Inode = sys.Ino
	}
	return st
}

// Load reads the notes document. A branch without notes returns nil, nil.
func (s *Store) Load() (*Document, error) {
	data, err := os.ReadFile(s.path(notesFile))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil //nolint:nilnil // no notes is a normal state, not an error
	}
	if err != nil {
		return nil, fmt.Errorf("read notes: %w", err)
	}
	doc, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", s.path(notesFile), err)
	}
	return doc, nil
}

// Update applies fn to the current document (an empty one when the branch
// has none) under the write lock and saves the result. fn's error aborts
// without writing.
func (s *Store) Update(fn func(doc *Document) error) (*Document, error) {
	var out *Document
	err := s.locked(func() error {
		doc, err := s.Load()
		if err != nil {
			return err
		}
		if doc == nil {
			doc = &Document{Format: Format}
		}
		if err := fn(doc); err != nil {
			return err
		}
		if err := s.save(doc); err != nil {
			return err
		}
		out = doc
		return nil
	})
	return out, err
}

// Reply records the reviewer's reply to note id: it is appended to the note's
// thread as pending and to the outbox with the note and earlier turns as
// context, in one locked step. Returns the updated document.
func (s *Store) Reply(id, body string) (*Document, error) {
	body = strings.TrimSpace(body)
	if body == "" {
		return nil, errors.New("reply is empty")
	}
	return s.Update(func(doc *Document) error {
		note, path := doc.Find(id)
		if note == nil {
			return fmt.Errorf("note %q not found", id)
		}
		item := InboxItem{Note: note.ref(path), Thread: note.bareThread()}
		turn := note.addTurn(AuthorYou, body, s.now().UTC())
		item.Reply = ReplyRef{ID: turn.ID, Body: turn.Body, At: turn.At}
		return s.appendOutbox(item)
	})
}

// InboxItem is one reviewer reply as handed to the agent: the note it answers,
// the thread before it and the reply itself.
type InboxItem struct {
	Note   NoteRef  `json:"note"`
	Thread []Turn   `json:"thread"`
	Reply  ReplyRef `json:"reply"`
}

// NoteRef is the note a reply belongs to, with enough context to answer it.
type NoteRef struct {
	ID       string `json:"id"`
	File     string `json:"file"`
	Line     int    `json:"line,omitempty"`
	Side     string `json:"side,omitempty"`
	Kind     Kind   `json:"kind"`
	Body     string `json:"body"`
	Outdated bool   `json:"outdated"`
}

// ReplyRef is the reviewer's reply.
type ReplyRef struct {
	ID   string    `json:"id"`
	Body string    `json:"body"`
	At   time.Time `json:"at"`
}

// Inbox returns the reviewer replies the agent has not consumed yet, oldest
// first, and marks them consumed: the outbox read position advances and the
// replies' thread turns move from pending to read. A reply the agent already
// answered unprompted (`note reply`) is skipped. Only whole lines are read, so
// a reply being appended is picked up next time.
func (s *Store) Inbox() ([]InboxItem, error) {
	var items []InboxItem
	err := s.locked(func() error {
		offset := s.readOffset()
		data, err := s.readOutboxFrom(offset)
		if err != nil {
			return err
		}
		end := bytes.LastIndexByte(data, '\n')
		if end < 0 {
			return nil
		}
		doc, err := s.Load()
		if err != nil {
			return err
		}
		changed := false
		sc := bufio.NewScanner(bytes.NewReader(data[:end+1]))
		sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
		for sc.Scan() {
			var item InboxItem
			if uerr := json.Unmarshal(sc.Bytes(), &item); uerr != nil {
				continue // a corrupt line must not wedge the inbox forever
			}
			if t := doc.turn(item.Note.ID, item.Reply.ID); t != nil {
				if t.State == TurnAnswered {
					continue
				}
				if t.State == TurnPending {
					t.State = TurnRead
					changed = true
				}
			}
			items = append(items, item)
		}
		if changed {
			if err := s.save(doc); err != nil {
				return err
			}
		}
		return s.writeOffset(offset + int64(end) + 1)
	})
	return items, err
}

// save writes doc atomically, stamping its update time. The caller holds the lock.
func (s *Store) save(doc *Document) error {
	doc.Format = Format
	doc.Updated = s.now().UTC()
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return fmt.Errorf("encode notes: %w", err)
	}
	if err := fsutil.AtomicWriteFile(s.path(notesFile), append(data, '\n')); err != nil {
		return fmt.Errorf("write notes: %w", err)
	}
	return nil
}

// appendOutbox appends one reply record. The caller holds the lock.
func (s *Store) appendOutbox(item InboxItem) error {
	line, err := json.Marshal(item)
	if err != nil {
		return fmt.Errorf("encode reply: %w", err)
	}
	f, err := os.OpenFile(s.path(outboxFile), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("open outbox: %w", err)
	}
	if _, err := f.Write(append(line, '\n')); err != nil {
		_ = f.Close()
		return fmt.Errorf("write outbox: %w", err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close outbox: %w", err)
	}
	return nil
}

func (s *Store) readOutboxFrom(offset int64) ([]byte, error) {
	f, err := os.Open(s.path(outboxFile))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("open outbox: %w", err)
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("stat outbox: %w", err)
	}
	if offset > fi.Size() {
		offset = 0 // the outbox was replaced; read it from the start
	}
	if _, serr := f.Seek(offset, io.SeekStart); serr != nil {
		return nil, fmt.Errorf("seek outbox: %w", serr)
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return nil, fmt.Errorf("read outbox: %w", err)
	}
	return data, nil
}

func (s *Store) readOffset() int64 {
	data, err := os.ReadFile(s.path(offsetFile))
	if err != nil {
		return 0
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

func (s *Store) writeOffset(n int64) error {
	if err := fsutil.AtomicWriteFile(s.path(offsetFile), []byte(strconv.FormatInt(n, 10)+"\n")); err != nil {
		return fmt.Errorf("write inbox offset: %w", err)
	}
	return nil
}

// locked runs fn holding the exclusive notes lock, creating the branch
// directory when needed.
func (s *Store) locked(fn func() error) error {
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return fmt.Errorf("create notes directory: %w", err)
	}
	f, err := os.OpenFile(s.path(lockFile), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("open notes lock: %w", err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return fmt.Errorf("lock notes: %w", err)
	}
	defer func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN) }()
	return fn()
}

func (s *Store) path(name string) string {
	return filepath.Join(s.dir, name)
}

// ref summarizes a note for an inbox item.
func (n Note) ref(path string) NoteRef {
	return NoteRef{ID: n.ID, File: path, Line: n.Line, Side: n.Side, Kind: n.Kind, Body: n.Body, Outdated: n.Status == StatusOutdated}
}

// bareThread copies the thread without the reply states, which mean nothing to
// the agent reading an inbox item.
func (n Note) bareThread() []Turn {
	out := make([]Turn, len(n.Thread))
	for i, t := range n.Thread {
		t.State = ""
		out[i] = t
	}
	return out
}

// addTurn appends a turn to the note's thread and returns it. Turn ids are
// "<note id>.<n>", unique within the document.
func (n *Note) addTurn(author, body string, at time.Time) Turn {
	t := Turn{ID: n.ID + "." + strconv.Itoa(len(n.Thread)+1), Author: author, Body: body, At: at}
	if author == AuthorYou {
		t.State = TurnPending
	}
	n.Thread = append(n.Thread, t)
	return t
}

// turn returns the thread turn id of note noteID, or nil.
func (d *Document) turn(noteID, id string) *Turn {
	note, _ := d.Find(noteID)
	if note == nil {
		return nil
	}
	for i := range note.Thread {
		if note.Thread[i].ID == id {
			return &note.Thread[i]
		}
	}
	return nil
}
