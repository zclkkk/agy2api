package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/oauth2"
)

func TestConcurrentRefreshPersistsRotatedTokenOnce(t *testing.T) {
	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if err := r.ParseForm(); err != nil {
			t.Error(err)
		}
		if r.Form.Get("refresh_token") != "refresh-old" {
			t.Error("incorrect refresh token")
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"access_token":"new-access","refresh_token":"refresh-new","expires_in":3600,"token_type":"Bearer"}`)
	}))
	defer up.Close()
	path := filepath.Join(t.TempDir(), "credentials.json")
	if err := Save(path, Credentials{ProjectID: "project", Token: oauth2.Token{AccessToken: "expired", RefreshToken: "refresh-old", Expiry: time.Now().Add(-time.Hour)}}); err != nil {
		t.Fatal(err)
	}
	m, err := Load(path, up.Client())
	if err != nil {
		t.Fatal(err)
	}
	m.config.Endpoint.TokenURL = up.URL
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			token, err := m.AccessToken(context.Background())
			if err != nil || token != "new-access" {
				t.Errorf("%q %v", token, err)
			}
		}()
	}
	wg.Wait()
	if calls.Load() != 1 {
		t.Fatalf("refresh calls=%d", calls.Load())
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var creds Credentials
	if err := json.Unmarshal(data, &creds); err != nil {
		t.Fatal(err)
	}
	if creds.Token.RefreshToken != "refresh-new" {
		t.Fatal("rotated refresh token not saved")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("permissions=%v", info.Mode())
	}
}
