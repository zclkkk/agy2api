// Package securefile writes private application data atomically.
package securefile

import (
	"fmt"
	"os"
	"path/filepath"
)

// Write replaces path with data. The directory must exist: a write never
// recreates a directory that was removed, such as an account deleted while
// the service still refreshes its token.
func Write(path string, data []byte) error {
	dir := filepath.Dir(path)
	f, err := os.CreateTemp(dir, ".agy2api-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	defer f.Close()
	if err := f.Chmod(0600); err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		return err
	}
	if err := f.Sync(); err != nil {
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("replace private file: %w", err)
	}
	return nil
}
