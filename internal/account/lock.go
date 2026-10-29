package account

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"

	"github.com/gofrs/flock"
	"github.com/zclkkk/agy2api/internal/auth"
)

const lockFile = ".lock" // a dot entry, which List skips

// local serializes this process's changes to account files; the file lock
// serializes them with other processes.
var local sync.Mutex

// withLock runs fn holding the lock every change to the account files under
// root takes: login, logout and the write-back of refreshed tokens. fn does
// only local file work, so the lock is never held across a network call. The
// operating system releases it if the process dies.
func withLock(root string, fn func() error) error {
	local.Lock()
	defer local.Unlock()
	if err := os.MkdirAll(root, 0700); err != nil {
		return err
	}
	l := flock.New(filepath.Join(root, lockFile))
	if err := l.Lock(); err != nil {
		return fmt.Errorf("lock the accounts: %w", err)
	}
	defer l.Unlock()
	return fn()
}

// Lock is the lock under which the account's refreshed tokens are written back.
func (a Account) Lock() auth.Lock {
	root := filepath.Dir(a.Dir)
	return func(fn func() error) error { return withLock(root, fn) }
}
