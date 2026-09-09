// Package status renders the piece state text exposed by .stats files.
package status

import (
	"fmt"
	"strings"
)

// PieceState is the filesystem-facing snapshot of one torrent piece.
type PieceState struct {
	Known    bool
	Complete bool
	Partial  bool
	Wanted   bool
	Checking bool
	Bytes    int64
}

// Render returns one token per piece, in input order, followed by a newline.
func Render(states []PieceState) string {
	if len(states) == 0 {
		return ""
	}
	tokens := make([]string, len(states))
	for i, state := range states {
		switch {
		case state.Known && state.Complete:
			tokens[i] = "[x]"
		case state.Known && state.Partial:
			tokens[i] = fmt.Sprintf("[X %d]", state.Bytes)
		case state.Known && !state.Checking && !state.Complete && !state.Partial && !state.Wanted:
			tokens[i] = "[N]"
		default:
			tokens[i] = "[]"
		}
	}
	return strings.Join(tokens, " ") + "\n"
}
