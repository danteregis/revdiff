package annotation

import (
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/umputun/revdiff/app/fsutil"
)

// Annotation represents a user comment on a specific diff line.
type Annotation struct {
	File    string `json:"file"`               // file path relative to repo root
	Line    int    `json:"line"`               // line number in the diff
	EndLine int    `json:"end_line,omitempty"` // end line of hunk range, 0 means no range
	Type    string `json:"type"`               // change type: "+", "-", or " "
	Comment string `json:"comment,omitempty"`  // user comment text; may be empty when Kind is set
	Kind    string `json:"kind,omitempty"`     // optional label from Kinds() (e.g. "praise"); empty for a plain comment
	// Status is the review state: open (the zero value), outdated (the code it
	// was written on changed), or resolved (closed by the reviewer).
	Status Status `json:"status,omitempty"`
	// Delivered is true once the annotation was handed to the agent (exit
	// output or an O flush) and has not been edited since. Delivered
	// annotations are not output again.
	Delivered bool `json:"delivered,omitempty"`
	// Anchor snapshots the annotated line so the annotation can be re-anchored
	// after the code changes; nil for file-level annotations and when unknown.
	Anchor *Anchor `json:"anchor,omitempty"`
}

// Status is the review state of an annotation.
type Status string

// annotation states. StatusOpen is the zero value.
const (
	StatusOpen     Status = ""
	StatusOutdated Status = "outdated"
	StatusResolved Status = "resolved"
)

// Pending reports whether the annotation still has to be sent to the agent:
// open (not outdated or resolved) and not delivered since its last edit.
func (a Annotation) Pending() bool {
	return a.Status == StatusOpen && !a.Delivered
}

// Store holds annotations in memory, keyed by filename.
type Store struct {
	annotations map[string][]Annotation
}

// NewStore creates a new empty annotation store.
func NewStore() *Store {
	return &Store{annotations: make(map[string][]Annotation)}
}

// Add adds an annotation for the given file and line.
// If an annotation already exists at the same file:line:type, it is replaced
// as a whole (comment, range, kind, status, delivery and anchor).
func (s *Store) Add(a Annotation) {
	existing := s.annotations[a.File]
	if i, ok := s.find(a.File, a.Line, a.Type); ok {
		existing[i] = a
		return
	}
	s.annotations[a.File] = append(existing, a)
}

// ReplaceFile replaces every annotation of file with anns (all of which must
// belong to file). An empty anns removes the file.
func (s *Store) ReplaceFile(file string, anns []Annotation) {
	if len(anns) == 0 {
		delete(s.annotations, file)
		return
	}
	s.annotations[file] = append([]Annotation(nil), anns...)
}

// PendingCount returns the number of annotations FormatOutput would emit.
func (s *Store) PendingCount() int {
	count := 0
	for _, anns := range s.annotations {
		for _, a := range anns {
			if a.Pending() {
				count++
			}
		}
	}
	return count
}

// PendingFiles returns the files that have pending annotations, sorted.
func (s *Store) PendingFiles() []string {
	var files []string
	for _, file := range s.Files() {
		for _, a := range s.annotations[file] {
			if a.Pending() {
				files = append(files, file)
				break
			}
		}
	}
	return files
}

// MarkDelivered marks every pending annotation delivered and returns how many
// were marked.
func (s *Store) MarkDelivered() int {
	n := 0
	for _, anns := range s.annotations {
		for i := range anns {
			if anns[i].Pending() {
				anns[i].Delivered = true
				n++
			}
		}
	}
	return n
}

// DiscardPending deletes every pending annotation and returns how many were
// removed; delivered, outdated and resolved annotations are kept.
func (s *Store) DiscardPending() int {
	n := 0
	for file, anns := range s.annotations {
		kept := make([]Annotation, 0, len(anns))
		for _, a := range anns {
			if a.Pending() {
				n++
				continue
			}
			kept = append(kept, a)
		}
		if len(kept) == 0 {
			delete(s.annotations, file)
			continue
		}
		s.annotations[file] = kept
	}
	return n
}

// Delete removes the annotation at the given file, line and change type.
// Returns true if an annotation was found and removed.
func (s *Store) Delete(file string, line int, changeType string) bool {
	i, ok := s.find(file, line, changeType)
	if !ok {
		return false
	}
	existing := s.annotations[file]
	s.annotations[file] = append(existing[:i], existing[i+1:]...)
	if len(s.annotations[file]) == 0 {
		delete(s.annotations, file)
	}
	return true
}

// Has checks if an annotation exists at the given file, line and change type.
func (s *Store) Has(file string, line int, changeType string) bool {
	_, ok := s.find(file, line, changeType)
	return ok
}

// find returns the index of an annotation matching file, line, and changeType.
func (s *Store) find(file string, line int, changeType string) (int, bool) {
	for i, a := range s.annotations[file] {
		if a.Line == line && a.Type == changeType {
			return i, true
		}
	}
	return 0, false
}

// Get returns all annotations for the given file, sorted by line number.
func (s *Store) Get(file string) []Annotation {
	result := make([]Annotation, len(s.annotations[file]))
	copy(result, s.annotations[file])
	s.sortByLine(result)
	return result
}

// sortByLine orders annotations by line, then change type, so equal lines on
// the old and new side (e.g. "-5" and "+5") always come out in the same order.
func (s *Store) sortByLine(anns []Annotation) {
	sort.SliceStable(anns, func(i, j int) bool {
		if anns[i].Line != anns[j].Line {
			return anns[i].Line < anns[j].Line
		}
		return anns[i].Type < anns[j].Type
	})
}

// Count returns the total number of annotations across all files.
func (s *Store) Count() int {
	count := 0
	for _, anns := range s.annotations {
		count += len(anns)
	}
	return count
}

// Clear removes all annotations from the store.
func (s *Store) Clear() {
	s.annotations = make(map[string][]Annotation)
}

// All returns all annotations grouped by file. The returned map is a copy.
func (s *Store) All() map[string][]Annotation {
	result := make(map[string][]Annotation, len(s.annotations))
	for file, anns := range s.annotations {
		copied := make([]Annotation, len(anns))
		copy(copied, anns)
		s.sortByLine(copied)
		result[file] = copied
	}
	return result
}

// Files returns the list of files that have annotations, sorted alphabetically.
func (s *Store) Files() []string {
	files := make([]string, 0, len(s.annotations))
	for file := range s.annotations {
		files = append(files, file)
	}
	sort.Strings(files)
	return files
}

// Load parses markdown produced by FormatOutput from r and adds each recovered
// annotation via Add (so duplicate file/line/type pairs apply last-write-wins).
// It is the symmetric inverse of FormatOutput on the API surface; callers that
// need to filter records (e.g. drop orphans against a diff) should use Parse
// directly and Add the survivors themselves.
func (s *Store) Load(r io.Reader) error {
	records, err := Parse(r)
	if err != nil {
		return err
	}
	for _, a := range records {
		s.Add(a)
	}
	return nil
}

// FormatOutput produces the structured output format for stdout.
// Files are sorted alphabetically, annotations within each file by line number.
// Only pending annotations (open and not yet delivered, see Annotation.Pending)
// are emitted: outdated and resolved annotations are never output, and a
// delivered one is output again only after it is edited. Returns empty string
// when nothing is pending.
//
// Body lines that start with "## " (the record-header form) are prefixed with a
// single space on output so parsers that split on "## " record headers cannot
// mistake a comment line for a new record. The added space is cosmetic
// (markdown renderers treat leading whitespace before a heading marker as
// paragraph text) and preserves the original text when whitespace is trimmed
// per line. Other markdown heading forms like "### subheader" are not escaped.
//
// A typed annotation carries its Kind as a Conventional Comments label at the
// start of the body ("kind: text", or the bare "kind" for an empty comment);
// the record header grammar is unchanged and untyped bodies are emitted as-is.
func (s *Store) FormatOutput() string {
	return s.format(Annotation.Pending)
}

// FormatOpen is FormatOutput for every open annotation, delivered or not:
// outdated and resolved annotations are still never emitted. It backs
// re-sending a whole review (--print-annotations=all).
func (s *Store) FormatOpen() string {
	return s.format(func(a Annotation) bool { return a.Status == StatusOpen })
}

// format emits the annotations include accepts in the FormatOutput format.
func (s *Store) format(include func(Annotation) bool) string {
	var buf strings.Builder
	first := true
	for _, file := range s.Files() {
		anns := s.Get(file) // sorted by line: file-level (0) first, then ascending
		for _, a := range anns {
			if !include(a) {
				continue
			}
			if !first {
				buf.WriteString("\n")
			}
			first = false
			body := s.escapeHeaderLines(s.labelBody(a.Kind, a.Comment))
			switch {
			case a.Line == 0:
				fmt.Fprintf(&buf, "## %s (file-level)\n%s\n", a.File, body)
			case a.EndLine > 0:
				fmt.Fprintf(&buf, "## %s:%d-%d (%s)\n%s\n", a.File, a.Line, a.EndLine, a.Type, body)
			default:
				fmt.Fprintf(&buf, "## %s:%d (%s)\n%s\n", a.File, a.Line, a.Type, body)
			}
		}
	}
	return buf.String()
}

// WriteFile writes FormatOutput to path atomically (temp file + rename, mode
// 0o600) and returns the exact snapshot written, so a concurrent reader sees
// either the old or the new complete file, never a truncated one.
func (s *Store) WriteFile(path string) (string, error) {
	content := s.FormatOutput()
	if err := fsutil.AtomicWriteFile(path, []byte(content)); err != nil {
		return "", fmt.Errorf("write annotations to %s: %w", path, err)
	}
	return content, nil
}

// escapeHeaderLines prefixes any body line whose first non-space content is
// "## " with a single extra space. The parser inverts this by stripping one
// leading space from any body line that, after left-trimming, begins with
// "## ". Escaping pre-indented variants (e.g. " ## ") keeps the round-trip
// symmetric for arbitrary user content. Other heading forms like "### " are
// not escaped since they cannot collide with the record-header split marker.
func (s *Store) escapeHeaderLines(body string) string {
	if !strings.Contains(body, "## ") {
		return body
	}
	lines := strings.Split(body, "\n")
	for i, line := range lines {
		if strings.HasPrefix(strings.TrimLeft(line, " "), "## ") {
			lines[i] = " " + line
		}
	}
	return strings.Join(lines, "\n")
}
