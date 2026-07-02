package fsatomic

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteFileCreatesFreshFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "data.json")
	data := []byte(`{"ok":true}`)
	if err := WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(data) {
		t.Fatalf("content = %q, want %q", got, data)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o, want %o", info.Mode().Perm(), 0o600)
	}
}

func TestWriteFileOverwritesExistingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "data.json")
	if err := WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WriteFile(path, []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "new" {
		t.Fatalf("content = %q, want %q", got, "new")
	}
}

func TestWriteFileCreatesParentDirectories(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "nested", "dir", "data.json")
	if err := WriteFile(path, []byte("ok"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "ok" {
		t.Fatalf("content = %q, want %q", got, "ok")
	}
}

func TestWriteFileLeavesNoTempLitter(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "data.json")
	if err := WriteFile(path, []byte("ok"), 0o644); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".ship-tmp-") {
			t.Fatalf("left temp file %q in %s", entry.Name(), dir)
		}
	}
}
