package notes

import (
	"os"
	"os/exec"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStore_Listener(t *testing.T) {
	s := newTestStore(t)
	s.now = time.Now
	assert.False(t, s.ListenerAlive())

	require.NoError(t, s.MarkListening())
	assert.True(t, s.ListenerAlive())
	require.NoError(t, s.MarkListening(), "a heartbeat on an owned marker")
	assert.True(t, s.ListenerAlive())

	s.UnmarkListening()
	assert.False(t, s.ListenerAlive())
	_, err := os.Stat(s.path(listenerFile))
	assert.True(t, os.IsNotExist(err))
}

func TestStore_ListenerStaleHeartbeat(t *testing.T) {
	s := newTestStore(t)
	s.now = time.Now
	require.NoError(t, s.MarkListening())
	old := time.Now().Add(-time.Minute)
	require.NoError(t, os.Chtimes(s.path(listenerFile), old, old))
	assert.False(t, s.ListenerAlive(), "a heartbeat older than the window is not listening")
	assert.False(t, s.ViewerAlive())
}

func TestStore_MarkerDeadProcess(t *testing.T) {
	s := newTestStore(t)
	s.now = time.Now
	cmd := exec.Command("true")
	require.NoError(t, cmd.Run())
	require.NoError(t, os.MkdirAll(s.dir, 0o700))
	require.NoError(t, os.WriteFile(s.path(viewerFile), []byte(strconv.Itoa(cmd.Process.Pid)+"\n"), 0o600))
	assert.False(t, s.ViewerAlive(), "a fresh marker of an exited process is not alive")

	s.UnmarkViewing()
	_, err := os.Stat(s.path(viewerFile))
	require.NoError(t, err, "a marker owned by another pid is left alone")

	require.NoError(t, os.WriteFile(s.path(viewerFile), []byte("garbage"), 0o600))
	assert.False(t, s.ViewerAlive())
	require.NoError(t, s.MarkViewing(), "a foreign or corrupt marker is taken over")
	assert.True(t, s.ViewerAlive())
	s.UnmarkViewing()
	assert.False(t, s.ViewerAlive())
}
