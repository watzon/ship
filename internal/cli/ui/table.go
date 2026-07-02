package ui

import (
	"fmt"
	"io"
	"strings"
)

type Table struct {
	style   Style
	headers []string
	rows    [][]string
}

func NewTable(w io.Writer) *Table {
	return &Table{style: NewStyle(w)}
}

func (t *Table) SetHeaders(headers ...string) {
	t.headers = append([]string(nil), headers...)
}

func (t *Table) AddRow(cells ...string) {
	t.rows = append(t.rows, append([]string(nil), cells...))
}

func (t *Table) Render(w io.Writer) {
	if len(t.headers) == 0 {
		return
	}
	widths := make([]int, len(t.headers))
	for i, header := range t.headers {
		widths[i] = len(header)
	}
	for _, row := range t.rows {
		for i, cell := range row {
			if i >= len(widths) {
				break
			}
			if len(cell) > widths[i] {
				widths[i] = len(cell)
			}
		}
	}

	var b strings.Builder
	for i, header := range t.headers {
		if i > 0 {
			b.WriteByte(' ')
		}
		b.WriteString(t.style.Teal(padRight(header, widths[i])))
	}
	b.WriteByte('\n')
	for _, row := range t.rows {
		for i := range t.headers {
			if i > 0 {
				b.WriteByte(' ')
			}
			cell := "-"
			if i < len(row) && strings.TrimSpace(row[i]) != "" {
				cell = row[i]
			}
			b.WriteString(t.style.White(padRight(cell, widths[i])))
		}
		b.WriteByte('\n')
	}
	fmt.Fprint(w, b.String())
}

func padRight(text string, width int) string {
	if visibleLength(text) >= width {
		return text
	}
	return text + strings.Repeat(" ", width-visibleLength(text))
}

func visibleLength(text string) int {
	length := 0
	for i := 0; i < len(text); {
		if text[i] == '\x1b' {
			for i < len(text) && text[i] != 'm' {
				i++
			}
			if i < len(text) {
				i++
			}
			continue
		}
		length++
		i++
	}
	return length
}