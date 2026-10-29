package account

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zclkkk/agy2api/internal/auth"
	"golang.org/x/oauth2"
)

func creds(project, refresh string) auth.Credentials {
	return auth.Credentials{ProjectID: project, Token: oauth2.Token{AccessToken: "access-" + refresh, RefreshToken: refresh}}
}

func savedCreds(t *testing.T, a Account) (string, string) {
	t.Helper()
	m, err := auth.Load(a.CredentialsPath(), nil)
	if err != nil {
		t.Fatal(err)
	}
	token, err := os.ReadFile(a.CredentialsPath())
	if err != nil {
		t.Fatal(err)
	}
	return m.ProjectID(), string(token)
}

func TestIDs(t *testing.T) {
	id := ID("104")
	if id != ID("104") || id == ID("105") || !ValidID(id) {
		t.Fatalf("unstable or invalid ID %q", id)
	}
	for _, bad := range []string{"", "a3f9c2e", "a3f9c2e1a", "A3F9C2E1", "a3f9c2eg", "../a3f9c", ".a3f9c2e"} {
		if ValidID(bad) {
			t.Fatalf("accepted ID %q", bad)
		}
	}
}

func TestLoginCreatesThenReauthenticates(t *testing.T) {
	dataDir := t.TempDir()
	user := auth.User{ID: "104", Email: "alice@example.com", Name: "Alice"}
	a, created, err := Login(dataDir, user, creds("project-a", "refresh-1"))
	if err != nil || !created {
		t.Fatalf("created=%v err=%v", created, err)
	}
	if a.ID != ID("104") || a.Dir != filepath.Join(dataDir, "accounts", a.ID) || a.Email != user.Email || a.Name != user.Name {
		t.Fatalf("account %+v", a)
	}
	if !strings.HasPrefix(a.APIKey, "agy_") || len(a.APIKey) != 47 {
		t.Fatalf("key %q", a.APIKey)
	}
	for path, mode := range map[string]os.FileMode{a.Dir: 0700, a.CredentialsPath(): 0600, filepath.Join(a.Dir, infoFile): 0600} {
		if info, err := os.Stat(path); err != nil || info.Mode().Perm() != mode {
			t.Fatalf("%s: mode %v err %v", path, info.Mode().Perm(), err)
		}
	}

	// A later login of the same Google account replaces the credentials and
	// display details but keeps the key.
	user.Email, user.Name = "alice@new.example.com", ""
	again, created, err := Login(dataDir, user, creds("project-a", "refresh-2"))
	if err != nil || created || again.APIKey != a.APIKey || again.Email != user.Email || again.Name != "" || !again.CreatedAt.Equal(a.CreatedAt) {
		t.Fatalf("relogin created=%v err=%v account=%+v", created, err, again)
	}
	if _, token := savedCreds(t, again); !strings.Contains(token, "refresh-2") {
		t.Fatal("relogin kept the old credentials")
	}

	// The state database is bound to the project, so a changed project needs
	// a fresh account.
	if _, _, err := Login(dataDir, user, creds("project-b", "refresh-3")); err == nil || !strings.Contains(err.Error(), "logout "+a.ID) {
		t.Fatalf("project change accepted: %v", err)
	}
	if project, token := savedCreds(t, again); project != "project-a" || !strings.Contains(token, "refresh-2") {
		t.Fatal("rejected login replaced the credentials")
	}

	accounts, err := List(dataDir)
	if err != nil || len(accounts) != 1 || accounts[0].Info != again.Info {
		t.Fatalf("list %+v err %v", accounts, err)
	}
}

// Two Google accounts whose IDs share the 8 hex digits are rejected rather
// than merged. A collision takes about 2^16 attempts to find.
func TestLoginRejectsIDCollision(t *testing.T) {
	seen := map[string]string{}
	var first, second string
	for i := 0; second == ""; i++ {
		userID := fmt.Sprint(i)
		if prior, ok := seen[ID(userID)]; ok {
			first, second = prior, userID
		}
		seen[ID(userID)] = userID
	}
	dataDir := t.TempDir()
	// Both share a project, as Workspace users may, so only the user check
	// tells them apart.
	if _, _, err := Login(dataDir, auth.User{ID: first, Email: "first@example.com"}, creds("p", "r1")); err != nil {
		t.Fatal(err)
	}
	if _, _, err := Login(dataDir, auth.User{ID: second, Email: "second@example.com"}, creds("p", "r2")); err == nil || !strings.Contains(err.Error(), "already belongs to first@example.com") {
		t.Fatalf("colliding account accepted: %v", err)
	}
}

func TestFailedLoginLeavesNoAccount(t *testing.T) {
	dataDir := t.TempDir()
	// Without a refresh token the credentials cannot be saved.
	if _, _, err := Login(dataDir, auth.User{ID: "104", Email: "a@example.com"}, auth.Credentials{ProjectID: "p"}); err == nil {
		t.Fatal("incomplete credentials accepted")
	}
	entries, err := os.ReadDir(filepath.Join(dataDir, "accounts"))
	if err != nil || len(entries) != 0 {
		t.Fatalf("failed login left %v (%v)", entries, err)
	}
}

func TestListOrderAndDamage(t *testing.T) {
	dataDir := t.TempDir()
	var ids []string
	for i, email := range []string{"b@example.com", "a@example.com"} {
		a, _, err := Login(dataDir, auth.User{ID: fmt.Sprint(200 + i), Email: email}, creds("p", "r"))
		if err != nil {
			t.Fatal(err)
		}
		// Creation times have second resolution; make the order explicit.
		a.CreatedAt = a.CreatedAt.Add(time.Duration(i) * time.Hour)
		if err := writeInfo(a.Dir, a.Info); err != nil {
			t.Fatal(err)
		}
		ids = append(ids, a.ID)
	}
	if err := os.Mkdir(filepath.Join(dataDir, "accounts", ".login-stale"), 0700); err != nil {
		t.Fatal(err)
	}
	accounts, err := List(dataDir)
	if err != nil || len(accounts) != 2 || accounts[0].ID != ids[0] || accounts[1].ID != ids[1] {
		t.Fatalf("list %+v err %v", accounts, err)
	}

	// Each damage returns the text the error must name and how to undo it.
	accountsDir := filepath.Join(dataDir, "accounts")
	first := filepath.Join(accounts[0].Dir, infoFile)
	moveAway := func(path string) func() {
		if err := os.Rename(path, path+".moved"); err != nil {
			t.Fatal(err)
		}
		return func() { os.Rename(path+".moved", path) }
	}
	create := func(path string, dir bool) func() {
		var err error
		if dir {
			err = os.Mkdir(path, 0700)
		} else {
			err = os.WriteFile(path, []byte("x"), 0600)
		}
		if err != nil {
			t.Fatal(err)
		}
		return func() { os.RemoveAll(path) }
	}
	for name, damage := range map[string]func() (string, func()){
		"stray file":          func() (string, func()) { return "notes.txt", create(filepath.Join(accountsDir, "notes.txt"), false) },
		"invalid name":        func() (string, func()) { return "qingkong", create(filepath.Join(accountsDir, "qingkong"), true) },
		"missing info":        func() (string, func()) { return ids[0], moveAway(first) },
		"missing credentials": func() (string, func()) { return ids[0], moveAway(accounts[0].CredentialsPath()) },
		"foreign info": func() (string, func()) {
			original, _ := os.ReadFile(first)
			foreign, _ := os.ReadFile(filepath.Join(accounts[1].Dir, infoFile))
			if err := os.WriteFile(first, foreign, 0600); err != nil {
				t.Fatal(err)
			}
			return ids[0], func() { os.WriteFile(first, original, 0600) }
		},
	} {
		want, undo := damage()
		if _, err := List(dataDir); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: damage not reported with %q: %v", name, want, err)
		}
		undo()
	}
	if _, err := List(dataDir); err != nil {
		t.Fatalf("damage not undone: %v", err)
	}
	if accounts, err := List(t.TempDir()); err != nil || accounts != nil {
		t.Fatal("an empty data directory has no accounts")
	}
}

func TestRemove(t *testing.T) {
	dataDir := t.TempDir()
	a, _, err := Login(dataDir, auth.User{ID: "104", Email: "a@example.com"}, creds("p", "r"))
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"", "../accounts", "ffffffff"} {
		if _, err := Remove(dataDir, id); err == nil {
			t.Fatalf("removed %q", id)
		}
	}
	// A damaged account can still be removed.
	if err := os.Remove(filepath.Join(a.Dir, infoFile)); err != nil {
		t.Fatal(err)
	}
	removed, err := Remove(dataDir, a.ID)
	if err != nil || removed.ID != a.ID {
		t.Fatalf("remove %+v err %v", removed, err)
	}
	if _, err := os.Stat(a.Dir); !os.IsNotExist(err) {
		t.Fatal("account directory remains")
	}
}
