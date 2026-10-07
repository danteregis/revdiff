package notes

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// presence markers: a pid file whose modification time is the heartbeat.
const (
	listenerFile = "listener.pid"
	viewerFile   = "viewer.pid"
)

// staleness windows. The listener beats every couple of seconds, so half a
// minute without a beat means it is gone. The viewer beats on the TUI's notes
// poll, which stops while revdiff is suspended in $EDITOR, so it gets a much
// longer window; its pid check still catches a closed TUI at once.
const (
	listenerStaleAfter = 30 * time.Second
	viewerStaleAfter   = 10 * time.Minute
)

// MarkListening records that this process waits for replies (`revdiff inbox
// --wait`), or refreshes the heartbeat when it already did.
func (s *Store) MarkListening() error { return s.mark(listenerFile) }

// UnmarkListening removes the listener marker when this process owns it.
func (s *Store) UnmarkListening() { s.unmark(listenerFile) }

// ListenerAlive reports whether an agent is waiting for replies: the marker
// exists, its heartbeat is fresh and its process is running.
func (s *Store) ListenerAlive() bool { return s.alive(listenerFile, listenerStaleAfter) }

// MarkViewing records that a revdiff TUI shows this branch's notes, or
// refreshes its heartbeat.
func (s *Store) MarkViewing() error { return s.mark(viewerFile) }

// UnmarkViewing removes the viewer marker when this process owns it.
func (s *Store) UnmarkViewing() { s.unmark(viewerFile) }

// ViewerAlive reports whether a revdiff TUI currently shows this branch's notes.
func (s *Store) ViewerAlive() bool { return s.alive(viewerFile, viewerStaleAfter) }

// mark writes this process's pid into the marker, or only touches it when it
// already holds this pid (the cheap heartbeat path).
func (s *Store) mark(name string) error {
	path := s.path(name)
	now := s.now()
	if pid, ok := s.markerPID(path); ok && pid == os.Getpid() {
		if err := os.Chtimes(path, now, now); err == nil {
			return nil
		}
	}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return fmt.Errorf("create notes directory: %w", err)
	}
	if err := os.WriteFile(path, []byte(strconv.Itoa(os.Getpid())+"\n"), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", name, err)
	}
	return nil
}

// unmark removes the marker if it still names this process; another process
// that took it over keeps it.
func (s *Store) unmark(name string) {
	path := s.path(name)
	if pid, ok := s.markerPID(path); ok && pid == os.Getpid() {
		_ = os.Remove(path)
	}
}

// alive reports whether the marker is fresh and its process exists.
func (s *Store) alive(name string, staleAfter time.Duration) bool {
	path := s.path(name)
	fi, err := os.Stat(path)
	if err != nil || s.now().Sub(fi.ModTime()) > staleAfter {
		return false
	}
	pid, ok := s.markerPID(path)
	if !ok {
		return false
	}
	return s.processAlive(pid)
}

func (s *Store) markerPID(path string) (int, bool) {
	data, err := os.ReadFile(path) //nolint:gosec // path is inside the notes directory
	if err != nil {
		return 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return 0, false
	}
	return pid, true
}

// processAlive reports whether pid names a running process. EPERM means it
// exists but belongs to someone else.
func (s *Store) processAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}
