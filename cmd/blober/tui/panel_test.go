package tui

import (
	"os"
	"path/filepath"
	"testing"
)

func TestListLocalDirsFirstAndParent(t *testing.T) {
	tmp := t.TempDir()
	if err := os.Mkdir(filepath.Join(tmp, "dir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmp, "file.txt"), []byte("hello"), 0o600); err != nil {
		t.Fatal(err)
	}
	entries, err := listLocal(tmp)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) < 3 {
		t.Fatalf("entries = %#v, want parent, dir, and file", entries)
	}
	if entries[0].Name != ".." || !entries[0].parent {
		t.Fatalf("first entry = %#v, want parent", entries[0])
	}
	if entries[1].Name != "dir/" || !entries[1].IsDir {
		t.Fatalf("second entry = %#v, want dir", entries[1])
	}
}

func TestSortEntriesKeepsParentThenDirectoriesFirst(t *testing.T) {
	entries := []Entry{
		{Name: "z.txt", Path: "z.txt"},
		{Name: "b-dir", Path: "b-dir", IsDir: true},
		{Name: "..", Path: "", IsDir: true, parent: true},
		{Name: "a.txt", Path: "a.txt"},
		{Name: "a-dir/", Path: "a-dir/", IsDir: true},
	}

	sortEntries(entries)

	want := []string{"..", "a-dir/", "b-dir", "a.txt", "z.txt"}
	for i, entry := range entries {
		if entry.Name != want[i] {
			t.Fatalf("entries[%d] = %q, want %q in %#v", i, entry.Name, want[i], entries)
		}
	}
}

func TestParentBlobPrefix(t *testing.T) {
	tests := map[string]string{
		"logs/2026/05/": "logs/2026/",
		"logs/":         "",
		"":              "",
	}
	for input, want := range tests {
		if got := parentBlobPrefix(input); got != want {
			t.Fatalf("parentBlobPrefix(%q) = %q, want %q", input, got, want)
		}
	}
}
