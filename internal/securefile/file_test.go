package securefile

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

func TestWriteReplacesPrivatelyButNeverCreatesDirectories(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "file.json")
	for _, data := range []string{"first", "second"} {
		if err := Write(path, []byte(data)); err != nil {
			t.Fatal(err)
		}
	}
	got, err := os.ReadFile(path)
	info, _ := os.Stat(path)
	if err != nil || string(got) != "second" || info.Mode().Perm() != 0600 {
		t.Fatalf("content %q mode %v err %v", got, info.Mode().Perm(), err)
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Fatalf("temporary files left: %v", entries)
	}
	missing := filepath.Join(dir, "removed", "file.json")
	if err := Write(missing, []byte("x")); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("write into a missing directory: %v", err)
	}
	if _, err := os.Stat(filepath.Dir(missing)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatal("a missing directory was created")
	}
}
