// Package account stores the Antigravity accounts the service hosts. Each
// account has a directory under data_dir/accounts, named by an ID derived from
// the Google account, holding its OAuth credentials, its API key and its
// conversation state.
package account

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/zclkkk/agy2api/internal/auth"
	"github.com/zclkkk/agy2api/internal/securefile"
)

const (
	infoFile        = "account.json"
	credentialsFile = "credentials.json"
	keyPrefix       = "agy_"
)

// Info is an account's identity and API key. Email and Name are for display
// and refresh on every login.
type Info struct {
	ID        string    `json:"-"`
	APIKey    string    `json:"api_key"`
	UserID    string    `json:"user_id"`
	Email     string    `json:"email"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"created_at"`
}

// Account is an account directory.
type Account struct {
	Info
	Dir string
}

func (a Account) CredentialsPath() string { return filepath.Join(a.Dir, credentialsFile) }

// ID derives the account ID from the Google account ID, so a Google account
// keeps its ID across logins and email changes.
func ID(userID string) string {
	sum := sha256.Sum256([]byte("agy2api account\x00" + userID))
	return hex.EncodeToString(sum[:4])
}

// ValidID reports whether id has the form ID returns.
func ValidID(id string) bool {
	return len(id) == 8 && !strings.ContainsFunc(id, func(r rune) bool { return (r < '0' || r > '9') && (r < 'a' || r > 'f') })
}

func root(dataDir string) string { return filepath.Join(dataDir, "accounts") }

// List loads every account, oldest first. An entry that is not a complete
// account is an error, so a damaged directory is noticed rather than skipped.
// Dot entries are the temporary directories of logins.
func List(dataDir string) ([]Account, error) {
	entries, err := os.ReadDir(root(dataDir))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var accounts []Account
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, ".") {
			continue
		}
		path := filepath.Join(root(dataDir), name)
		if !entry.IsDir() || !ValidID(name) {
			return nil, fmt.Errorf("%s is not an account directory; remove it", path)
		}
		a, err := load(path, name)
		if err != nil {
			return nil, fmt.Errorf("account %s: %w", name, err)
		}
		accounts = append(accounts, a)
	}
	slices.SortFunc(accounts, func(a, b Account) int {
		if c := a.CreatedAt.Compare(b.CreatedAt); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	})
	return accounts, nil
}

func load(dir, id string) (Account, error) {
	data, err := os.ReadFile(filepath.Join(dir, infoFile))
	if err != nil {
		return Account{}, fmt.Errorf("read %s: %w", infoFile, err)
	}
	var info Info
	if err := json.Unmarshal(data, &info); err != nil {
		return Account{}, fmt.Errorf("decode %s: %w", infoFile, err)
	}
	if info.APIKey == "" || info.UserID == "" || info.Email == "" {
		return Account{}, fmt.Errorf("%s is incomplete", infoFile)
	}
	if ID(info.UserID) != id {
		return Account{}, fmt.Errorf("%s belongs to account %s", infoFile, ID(info.UserID))
	}
	info.ID = id
	a := Account{Info: info, Dir: dir}
	if _, err := auth.Load(a.CredentialsPath(), nil); err != nil {
		return Account{}, err
	}
	return a, nil
}

// Login stores the credentials of a signed-in Google account. A new account
// gets a fresh API key; a known one has its credentials replaced and keeps its
// key. created reports which happened.
func Login(dataDir string, user auth.User, creds auth.Credentials) (a Account, created bool, err error) {
	id := ID(user.ID)
	dir := filepath.Join(root(dataDir), id)
	if _, err := os.Stat(dir); err == nil {
		return relogin(dir, id, user, creds)
	} else if !errors.Is(err, fs.ErrNotExist) {
		return Account{}, false, err
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return Account{}, false, err
	}
	info := Info{ID: id, APIKey: keyPrefix + base64.RawURLEncoding.EncodeToString(key), UserID: user.ID, Email: user.Email, Name: user.Name, CreatedAt: time.Now().UTC().Truncate(time.Second)}
	if err := os.MkdirAll(root(dataDir), 0700); err != nil {
		return Account{}, false, err
	}
	// The account appears complete or not at all: it is written into a
	// temporary directory that List skips, then renamed into place.
	tmp, err := os.MkdirTemp(root(dataDir), ".login-*")
	if err != nil {
		return Account{}, false, err
	}
	defer os.RemoveAll(tmp)
	if err := auth.Save(filepath.Join(tmp, credentialsFile), creds); err != nil {
		return Account{}, false, err
	}
	if err := writeInfo(tmp, info); err != nil {
		return Account{}, false, err
	}
	if err := os.Rename(tmp, dir); err != nil {
		return Account{}, false, fmt.Errorf("create account %s: %w", id, err)
	}
	return Account{Info: info, Dir: dir}, true, nil
}

func relogin(dir, id string, user auth.User, creds auth.Credentials) (Account, bool, error) {
	a, err := load(dir, id)
	if err != nil {
		return Account{}, false, fmt.Errorf("account %s is damaged (%w); run agy2api logout %s, then log in again", id, err, id)
	}
	if a.UserID != user.ID {
		return Account{}, false, fmt.Errorf("account ID %s already belongs to %s", id, a.Email)
	}
	// The state database is bound to the project the account had.
	previous, err := auth.Load(a.CredentialsPath(), nil)
	if err != nil {
		return Account{}, false, err
	}
	if previous.ProjectID() != creds.ProjectID {
		return Account{}, false, fmt.Errorf("the Antigravity project of %s changed; run agy2api logout %s, then log in again", a.Email, id)
	}
	if err := auth.Save(a.CredentialsPath(), creds); err != nil {
		return Account{}, false, err
	}
	a.Email, a.Name = user.Email, user.Name
	if err := writeInfo(dir, a.Info); err != nil {
		return Account{}, false, err
	}
	return a, false, nil
}

func writeInfo(dir string, info Info) error {
	data, err := json.MarshalIndent(info, "", "  ")
	if err != nil {
		return err
	}
	return securefile.Write(filepath.Join(dir, infoFile), data)
}

// Remove deletes an account's directory with its credentials, key and state.
// A damaged account can be removed too; its Info is then empty but for ID.
//
// The directory is first renamed to a dot entry, which List skips, so the
// account is gone at once and a deletion that fails partway leaves nothing
// that stops the service from starting. A running service still writes by
// the old path, which no longer exists.
func Remove(dataDir, id string) (Account, error) {
	if !ValidID(id) {
		return Account{}, fmt.Errorf("invalid account ID %q; run agy2api accounts to list them", id)
	}
	dir := filepath.Join(root(dataDir), id)
	if _, err := os.Stat(dir); errors.Is(err, fs.ErrNotExist) {
		return Account{}, fmt.Errorf("no account %s; run agy2api accounts to list them", id)
	} else if err != nil {
		return Account{}, err
	}
	a, err := load(dir, id)
	if err != nil {
		a = Account{Info: Info{ID: id}, Dir: dir}
	}
	suffix := make([]byte, 4)
	if _, err := rand.Read(suffix); err != nil {
		return Account{}, err
	}
	removed := filepath.Join(root(dataDir), ".removed-"+id+"-"+hex.EncodeToString(suffix))
	if err := os.Rename(dir, removed); err != nil {
		return Account{}, fmt.Errorf("remove account %s: %w", id, err)
	}
	if err := os.RemoveAll(removed); err != nil {
		return a, fmt.Errorf("account %s is removed, but deleting its data failed; delete %s: %w", id, removed, err)
	}
	return a, nil
}
