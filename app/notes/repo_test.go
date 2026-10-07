package notes

import (
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRepo(t *testing.T) {
	root := t.TempDir()
	r := NewRepo(func(branch string) string { return filepath.Join(root, branch) })

	doc, err := r.Load("feat")
	require.NoError(t, err)
	assert.Nil(t, doc)
	assert.Equal(t, Stamp{}, r.Stamp("feat"))

	_, err = New(filepath.Join(root, "feat")).Update(func(d *Document) error {
		_, aerr := d.AddNote("a.go", Note{Line: 1, Body: "x"})
		return aerr
	})
	require.NoError(t, err)
	assert.NotEqual(t, Stamp{}, r.Stamp("feat"))

	doc, err = r.Reply("feat", "n1", "question")
	require.NoError(t, err)
	assert.Equal(t, 1, doc.PendingReplies())
	_, err = r.Reply("other", "n1", "question")
	require.Error(t, err, "branches are separate")

	assert.False(t, r.ListenerAlive("feat"))
	require.NoError(t, New(filepath.Join(root, "feat")).MarkListening())
	assert.True(t, r.ListenerAlive("feat"))

	require.NoError(t, r.MarkViewing("feat"))
	require.NoError(t, r.MarkViewing("main"))
	assert.True(t, New(filepath.Join(root, "feat")).ViewerAlive())
	r.UnmarkViewing("main")
	assert.False(t, New(filepath.Join(root, "main")).ViewerAlive())
	r.Close()
	assert.False(t, New(filepath.Join(root, "feat")).ViewerAlive(), "Close clears every marker it set")

	var none *Repo
	assert.NotPanics(t, none.Close, "a nil Repo closes as a no-op")
}
