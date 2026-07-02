package ui

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
)

type UsageError struct {
	Message  string
	Usage    string
	Hints    []string
	Received int
	Expected int
}

func (e *UsageError) Error() string {
	return e.Message
}

func FormatError(err error) string {
	if err == nil {
		return ""
	}
	var usageErr *UsageError
	if errors.As(err, &usageErr) {
		return formatUsageError(usageErr)
	}
	return NewStyle(os.Stderr).Error("error: ") + err.Error()
}

func PrintError(w io.Writer, err error) {
	if err == nil {
		return
	}
	fmt.Fprintln(w, FormatError(err))
}

func formatUsageError(err *UsageError) string {
	style := NewStyle(os.Stderr)
	var b strings.Builder
	b.WriteString(style.Error("error: "))
	b.WriteString(style.White(err.Message))
	b.WriteByte('\n')
	if err.Usage != "" {
		b.WriteByte('\n')
		b.WriteString(style.Gray("  usage: "))
		b.WriteString(style.Teal(err.Usage))
		b.WriteByte('\n')
	}
	for _, hint := range err.Hints {
		b.WriteString(style.Gray("         "))
		b.WriteString(hint)
		b.WriteByte('\n')
	}
	return strings.TrimRight(b.String(), "\n")
}
