package ui

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestExactArgsMissingEnvironment(t *testing.T) {
	cmd := &cobra.Command{Use: "status ENV"}
	validate := ExactArgs(Env)
	err := validate(cmd, nil)
	if err == nil {
		t.Fatal("expected error")
	}
	var usageErr *UsageError
	if !errors.As(err, &usageErr) {
		t.Fatalf("expected UsageError, got %T: %v", err, err)
	}
	if usageErr.Message != "missing environment" {
		t.Fatalf("message = %q", usageErr.Message)
	}
	if usageErr.Usage != "status ENV" {
		t.Fatalf("usage = %q", usageErr.Usage)
	}
}

func TestFormatErrorUsage(t *testing.T) {
	text := FormatError(&UsageError{
		Message: "missing environment",
		Usage:   "ship status ENV",
		Hints:   []string{"ENV: environment name from ship.yml (e.g. production, staging)"},
	})
	if !strings.Contains(text, "missing environment") {
		t.Fatalf("formatted error = %q", text)
	}
	if !strings.Contains(text, "ship status ENV") {
		t.Fatalf("formatted error = %q", text)
	}
}

func TestTableRender(t *testing.T) {
	var buf bytes.Buffer
	table := NewTable(&buf)
	table.SetHeaders("HOST", "STATUS")
	table.AddRow("web-1", "Up 10 seconds")
	table.AddRow("web-2", "Exited")
	table.Render(&buf)
	text := buf.String()
	for _, needle := range []string{"HOST", "STATUS", "web-1", "Up 10 seconds", "web-2", "Exited"} {
		if !strings.Contains(text, needle) {
			t.Fatalf("table missing %q:\n%s", needle, text)
		}
	}
}

func TestStatusColorPlainWithoutTTY(t *testing.T) {
	style := NewStyle(&bytes.Buffer{})
	if style.StatusColor("Up 2 minutes") != "Up 2 minutes" {
		t.Fatal("expected plain text without color")
	}
}