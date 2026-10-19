package blueprint

import (
	"fmt"
	"strings"
)

// Problem is one thing wrong with a blueprint package. Where names the part
// of the package it is about, such as `settings "images"` or `repeats[0]`.
type Problem struct{ Where, Msg string }

func (p Problem) String() string {
	if p.Where == "" {
		return p.Msg
	}
	return p.Where + ": " + p.Msg
}

// Problems is every problem a check found, in the order the package declares
// the parts they are about. It is an error with one problem per line.
type Problems []Problem

func (ps Problems) Error() string {
	lines := make([]string, len(ps))
	for i, p := range ps {
		lines[i] = p.String()
	}
	return strings.Join(lines, "\n")
}

func (ps *Problems) add(where, format string, args ...any) {
	*ps = append(*ps, Problem{Where: where, Msg: fmt.Sprintf(format, args...)})
}
