package notes

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	s := New(filepath.Join(t.TempDir(), "branch"))
	clock := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	s.now = func() time.Time { clock = clock.Add(time.Second); return clock }
	return s
}

func seedStore(t *testing.T, s *Store) {
	t.Helper()
	_, err := s.Update(func(doc *Document) error {
		_, err := doc.AddNote("a.go", Note{Kind: KindCaution, Line: 2, Body: "careful here"})
		return err
	})
	require.NoError(t, err)
}

func TestStore_LoadMissing(t *testing.T) {
	s := newTestStore(t)
	doc, err := s.Load()
	require.NoError(t, err)
	assert.Nil(t, doc)
	assert.Equal(t, Stamp{}, s.Stamp())
}

func TestStore_UpdateAndStamp(t *testing.T) {
	s := newTestStore(t)
	seedStore(t, s)
	st1 := s.Stamp()
	assert.NotEqual(t, Stamp{}, st1)

	doc, err := s.Load()
	require.NoError(t, err)
	require.NotNil(t, doc)
	assert.Equal(t, Format, doc.Format)
	assert.False(t, doc.Updated.IsZero())

	_, err = s.Update(func(doc *Document) error {
		_, aerr := doc.Answer("n1", "more", s.now())
		return aerr
	})
	require.NoError(t, err)
	assert.NotEqual(t, st1, s.Stamp(), "every write changes the stamp")

	fi, err := os.Stat(s.path(notesFile))
	require.NoError(t, err)
	assert.Equal(t, os.FileMode(0o600), fi.Mode().Perm())

	// a failing update writes nothing
	before := s.Stamp()
	_, err = s.Update(func(*Document) error { return assert.AnError })
	require.ErrorIs(t, err, assert.AnError)
	assert.Equal(t, before, s.Stamp())
}

func TestStore_LoadCorrupt(t *testing.T) {
	s := newTestStore(t)
	require.NoError(t, os.MkdirAll(s.dir, 0o700))
	require.NoError(t, os.WriteFile(s.path(notesFile), []byte("{"), 0o600))
	_, err := s.Load()
	require.Error(t, err)
	_, err = s.Update(func(*Document) error { return nil })
	require.Error(t, err, "a corrupt file is never overwritten blindly")
}

func TestStore_ReplyAndInbox(t *testing.T) {
	s := newTestStore(t)
	seedStore(t, s)

	items, err := s.Inbox()
	require.NoError(t, err)
	assert.Empty(t, items, "no replies yet")

	doc, err := s.Reply("n1", " why is this careful? ")
	require.NoError(t, err)
	note, _ := doc.Find("n1")
	require.Len(t, note.Thread, 1)
	assert.Equal(t, TurnPending, note.Thread[0].State)
	assert.Equal(t, 1, doc.PendingReplies())

	_, err = s.Reply("n1", "and another")
	require.NoError(t, err)

	items, err = s.Inbox()
	require.NoError(t, err)
	require.Len(t, items, 2)
	assert.Equal(t, "n1", items[0].Note.ID)
	assert.Equal(t, "a.go", items[0].Note.File)
	assert.Equal(t, 2, items[0].Note.Line)
	assert.Equal(t, KindCaution, items[0].Note.Kind)
	assert.Equal(t, "careful here", items[0].Note.Body)
	assert.Empty(t, items[0].Thread)
	assert.Equal(t, "why is this careful?", items[0].Reply.Body)
	require.Len(t, items[1].Thread, 1, "the second reply carries the first as context")
	assert.Empty(t, items[1].Thread[0].State, "states are not part of the context")

	doc, err = s.Load()
	require.NoError(t, err)
	note, _ = doc.Find("n1")
	assert.Equal(t, TurnRead, note.Thread[0].State)
	assert.Equal(t, 2, doc.PendingReplies(), "read is still unanswered")

	items, err = s.Inbox()
	require.NoError(t, err)
	assert.Empty(t, items, "consumed replies are not handed out twice")

	_, err = s.Reply("missing", "x")
	require.Error(t, err)
	_, err = s.Reply("n1", "  ")
	require.Error(t, err)
}

func TestStore_InboxSkipsAnswered(t *testing.T) {
	s := newTestStore(t)
	seedStore(t, s)
	_, err := s.Reply("n1", "question")
	require.NoError(t, err)
	_, err = s.Update(func(doc *Document) error {
		_, aerr := doc.Answer("n1", "answered before the agent read its inbox", s.now())
		return aerr
	})
	require.NoError(t, err)
	items, err := s.Inbox()
	require.NoError(t, err)
	assert.Empty(t, items)
}

func TestStore_InboxPartialAndCorruptLines(t *testing.T) {
	s := newTestStore(t)
	seedStore(t, s)
	_, err := s.Reply("n1", "complete")
	require.NoError(t, err)
	f, err := os.OpenFile(s.path(outboxFile), os.O_APPEND|os.O_WRONLY, 0o600)
	require.NoError(t, err)
	_, err = f.WriteString("not json\n{\"note\":{\"id\":\"n1\"")
	require.NoError(t, err)
	require.NoError(t, f.Close())

	items, err := s.Inbox()
	require.NoError(t, err)
	require.Len(t, items, 1, "corrupt line skipped, partial line left for later")

	f, err = os.OpenFile(s.path(outboxFile), os.O_APPEND|os.O_WRONLY, 0o600)
	require.NoError(t, err)
	_, err = f.WriteString(",\"file\":\"a.go\"},\"reply\":{\"id\":\"x\",\"body\":\"late\"}}\n")
	require.NoError(t, err)
	require.NoError(t, f.Close())
	items, err = s.Inbox()
	require.NoError(t, err)
	require.Len(t, items, 1)
	assert.Equal(t, "late", items[0].Reply.Body, "a reply for an unknown turn is still delivered")
}

func TestStore_InboxOutboxReplaced(t *testing.T) {
	s := newTestStore(t)
	seedStore(t, s)
	require.NoError(t, s.writeOffset(1<<20))
	_, err := s.Reply("n1", "fresh")
	require.NoError(t, err)
	items, err := s.Inbox()
	require.NoError(t, err)
	require.Len(t, items, 1, "an offset past the end restarts from the top")
}

func TestStore_ConcurrentWriters(t *testing.T) {
	s := newTestStore(t)
	s.now = time.Now
	seedStore(t, s)
	other := New(filepath.Dir(s.dir)) // a second process writing the same branch
	var wg sync.WaitGroup
	for i := range 10 {
		wg.Go(func() {
			st := s
			if i%2 == 0 {
				st = other
			}
			_, err := st.Reply("n1", "reply")
			assert.NoError(t, err)
		})
	}
	wg.Wait()
	doc, err := s.Load()
	require.NoError(t, err)
	note, _ := doc.Find("n1")
	assert.Len(t, note.Thread, 10, "no reply lost to a concurrent write")
	items, err := s.Inbox()
	require.NoError(t, err)
	assert.Len(t, items, 10)
}
