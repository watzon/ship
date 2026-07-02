package ui

import (
	"io"
	"os"
	"strings"
)

// Brand colors from assets/logos/ship-logo.svg
const (
	ColorWhite    = "#F8F8F8"
	ColorNavy     = "#15375A"
	ColorTeal     = "#149B94"
	ColorTealDark = "#176C7D"
)

var (
	ansiWhite    = "\x1b[38;2;248;248;248m"
	ansiGray     = "\x1b[38;2;140;145;150m"
	ansiNavy     = "\x1b[38;2;21;55;90m"
	ansiTeal     = "\x1b[38;2;20;155;148m"
	ansiTealDark = "\x1b[38;2;23;108;125m"
	ansiRed      = "\x1b[38;2;220;80;80m"
	ansiGreen    = "\x1b[38;2;60;190;140m"
	ansiYellow   = "\x1b[38;2;220;180;60m"
	ansiReset    = "\x1b[0m"
	ansiBold     = "\x1b[1m"
)

type Style struct {
	enabled bool
}

func NewStyle(w io.Writer) Style {
	return Style{enabled: colorEnabled(w)}
}

func colorEnabled(w io.Writer) bool {
	if os.Getenv("NO_COLOR") != "" {
		return false
	}
	if f, ok := w.(*os.File); ok {
		return isTerminal(f)
	}
	return false
}

func (s Style) White(text string) string {
	if !s.enabled {
		return text
	}
	return ansiWhite + text + ansiReset
}

func (s Style) Gray(text string) string {
	if !s.enabled {
		return text
	}
	return ansiGray + text + ansiReset
}

func (s Style) Navy(text string) string {
	if !s.enabled {
		return text
	}
	return ansiNavy + text + ansiReset
}

func (s Style) Teal(text string) string {
	if !s.enabled {
		return text
	}
	return ansiTeal + text + ansiReset
}

func (s Style) TealDark(text string) string {
	if !s.enabled {
		return text
	}
	return ansiTealDark + text + ansiReset
}

func (s Style) Bold(text string) string {
	if !s.enabled {
		return text
	}
	return ansiBold + text + ansiReset
}

func (s Style) Error(text string) string {
	if !s.enabled {
		return text
	}
	return ansiRed + text + ansiReset
}

func (s Style) Success(text string) string {
	if !s.enabled {
		return text
	}
	return ansiGreen + text + ansiReset
}

func (s Style) Warn(text string) string {
	if !s.enabled {
		return text
	}
	return ansiYellow + text + ansiReset
}

func (s Style) AccentLabel(label, value string) string {
	return s.Teal(label) + s.White(value)
}

func (s Style) StatusColor(status string) string {
	lower := strings.ToLower(strings.TrimSpace(status))
	switch {
	case strings.HasPrefix(lower, "up"), strings.Contains(lower, "healthy"), lower == "running", lower == "ok":
		return s.Success(status)
	case strings.HasPrefix(lower, "exit"), strings.Contains(lower, "dead"), strings.Contains(lower, "fail"), strings.Contains(lower, "unhealthy"):
		return s.Error(status)
	case lower == "" || lower == "-":
		return s.Gray(status)
	default:
		return s.White(status)
	}
}
