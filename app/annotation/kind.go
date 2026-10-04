package annotation

import (
	"slices"
	"strings"
)

// KindPraise labels positive feedback that asks for no change.
const KindPraise = "praise"

// kinds is the single ordered list of known annotation kinds, modeled on
// Conventional Comments labels. The empty kind (plain comment) is not listed.
// Order is the UI cycling order; output and parsing only care about membership.
var kinds = []string{"bug", "suggestion", "question", "nitpick", KindPraise}

// Kinds returns the known annotation kinds in cycling order.
func Kinds() []string {
	return slices.Clone(kinds)
}

// labelBody serializes kind and comment into the record body: "kind: comment"
// for a typed comment, the bare "kind" for a typed empty comment, and the
// comment unchanged for an untyped one. Multi-line comments carry the label on
// the first line only.
func (s *Store) labelBody(kind, comment string) string {
	switch {
	case kind == "":
		return comment
	case comment == "":
		return kind
	default:
		return kind + ": " + comment
	}
}

// splitLabel is the inverse of labelBody. A first line that is exactly a known
// kind, or starts with a known kind followed by ":" and an optional single
// space, yields that kind and the remaining text. Anything else, including
// unknown labels, is returned as an untyped comment.
func (p *parser) splitLabel(body string) (kind, comment string) {
	first, rest, multi := strings.Cut(body, "\n")
	for _, k := range kinds {
		if first == k {
			return k, rest
		}
		after, ok := strings.CutPrefix(first, k+":")
		if !ok {
			continue
		}
		after = strings.TrimPrefix(after, " ")
		if multi {
			after += "\n" + rest
		}
		return k, after
	}
	return "", body
}
