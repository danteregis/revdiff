package notes

import (
	"sync"
)

// Repo gives the TUI the notes of any branch of one repository: each call
// resolves the branch directory and acts on that branch's Store. It remembers
// which branches it marked as viewed so Close can clear them when revdiff
// exits. Consumed through ui.NotesStore.
type Repo struct {
	dir func(branch string) string

	mu      sync.Mutex
	viewing map[string]bool
}

// NewRepo returns a Repo whose branch session directories come from dir
// (session.Store.BranchDir).
func NewRepo(dir func(branch string) string) *Repo {
	return &Repo{dir: dir, viewing: map[string]bool{}}
}

// Stamp returns the version stamp of branch's notes file.
func (r *Repo) Stamp(branch string) Stamp { return r.store(branch).Stamp() }

// Load reads branch's notes; nil when it has none.
func (r *Repo) Load(branch string) (*Document, error) { return r.store(branch).Load() }

// Reply records the reviewer's reply to note id of branch.
func (r *Repo) Reply(branch, id, body string) (*Document, error) {
	return r.store(branch).Reply(id, body)
}

// ListenerAlive reports whether an agent waits for branch's replies.
func (r *Repo) ListenerAlive(branch string) bool { return r.store(branch).ListenerAlive() }

// MarkViewing records (or refreshes) that this process shows branch's notes.
func (r *Repo) MarkViewing(branch string) error {
	r.mu.Lock()
	r.viewing[branch] = true
	r.mu.Unlock()
	return r.store(branch).MarkViewing()
}

// UnmarkViewing clears the viewer marker of branch.
func (r *Repo) UnmarkViewing(branch string) {
	r.mu.Lock()
	delete(r.viewing, branch)
	r.mu.Unlock()
	r.store(branch).UnmarkViewing()
}

// Close clears every viewer marker this Repo set, telling a waiting agent the
// review closed. A nil Repo (no notes in this mode) is a no-op.
func (r *Repo) Close() {
	if r == nil {
		return
	}
	r.mu.Lock()
	branches := make([]string, 0, len(r.viewing))
	for b := range r.viewing {
		branches = append(branches, b)
	}
	r.viewing = map[string]bool{}
	r.mu.Unlock()
	for _, b := range branches {
		r.store(b).UnmarkViewing()
	}
}

func (r *Repo) store(branch string) *Store { return New(r.dir(branch)) }
