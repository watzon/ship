package ui

import (
	"os"

	"golang.org/x/term"
)

func isTerminal(f *os.File) bool {
	return term.IsTerminal(int(f.Fd()))
}
