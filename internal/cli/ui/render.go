package ui

import (
	"fmt"
	"io"
	"strings"
)

type HeaderField struct {
	Label  string
	Value  string
	Accent bool
}

func PrintHeader(w io.Writer, environment string, fields ...HeaderField) {
	style := NewStyle(w)
	fmt.Fprint(w, style.Teal("environment "))
	fmt.Fprint(w, style.White(environment))
	for _, field := range fields {
		if strings.TrimSpace(field.Value) == "" {
			continue
		}
		fmt.Fprint(w, style.Gray("  "+field.Label+" "))
		if field.Accent {
			fmt.Fprint(w, style.Teal(field.Value))
		} else {
			fmt.Fprint(w, style.White(field.Value))
		}
	}
	fmt.Fprintln(w)
}

func PrintLine(w io.Writer, label, value string) {
	style := NewStyle(w)
	fmt.Fprint(w, style.Gray(label+" "))
	fmt.Fprintln(w, style.White(value))
}

func PrintSection(w io.Writer, title string) {
	style := NewStyle(w)
	fmt.Fprintln(w)
	fmt.Fprintln(w, style.TealDark(title))
}

func PrintNotice(w io.Writer, message string) {
	style := NewStyle(w)
	fmt.Fprintln(w, style.Gray(message))
}

func PrintOK(w io.Writer, message string) {
	style := NewStyle(w)
	fmt.Fprintln(w, style.Success(message))
}

func PrintWarn(w io.Writer, message string) {
	style := NewStyle(w)
	fmt.Fprintln(w, style.Warn(message))
}

func PrintErrorLine(w io.Writer, message string) {
	style := NewStyle(w)
	fmt.Fprintln(w, style.Error(message))
}

func RenderTable(w io.Writer, table *Table) {
	fmt.Fprintln(w)
	table.Render(w)
}

func Dash(value string) string {
	if strings.TrimSpace(value) == "" {
		return "-"
	}
	return value
}

func (s Style) PlacementState(state string) string {
	switch strings.ToLower(strings.TrimSpace(state)) {
	case "ok", "healthy", "running":
		return s.Success(state)
	case "missing", "wrong_release", "wrong_host", "failed", "unhealthy":
		return s.Error(state)
	case "planned", "skipped":
		return s.Gray(state)
	default:
		return s.White(state)
	}
}

func (s Style) CheckStatus(status string) string {
	switch strings.ToLower(strings.TrimSpace(status)) {
	case "ok", "planned":
		return s.Success(status)
	case "failed", "invalid":
		return s.Error(status)
	case "skipped":
		return s.Gray(status)
	default:
		return s.White(status)
	}
}
