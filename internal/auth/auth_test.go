package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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

func TestInvalidateForcesRefreshOnlyForCurrentToken(t *testing.T) {
	var calls atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"access_token":"next-access","expires_in":3600,"token_type":"Bearer"}`)
	}))
	defer up.Close()
	path := filepath.Join(t.TempDir(), "credentials.json")
	if err := Save(path, Credentials{ProjectID: "project", Token: oauth2.Token{AccessToken: "valid", RefreshToken: "refresh", Expiry: time.Now().Add(time.Hour)}}); err != nil {
		t.Fatal(err)
	}
	m, err := Load(path, up.Client())
	if err != nil {
		t.Fatal(err)
	}
	m.config.Endpoint.TokenURL = up.URL
	m.Invalidate("some-older-token")
	if token, _ := m.AccessToken(context.Background()); token != "valid" || calls.Load() != 0 {
		t.Fatalf("stale invalidation refreshed: %q", token)
	}
	m.Invalidate("valid")
	if token, err := m.AccessToken(context.Background()); err != nil || token != "next-access" || calls.Load() != 1 {
		t.Fatalf("%q %v calls=%d", token, err, calls.Load())
	}
}

type userTransport func(*http.Request) (*http.Response, error)

func (f userTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestFetchUserRequiresIDAndEmail(t *testing.T) {
	for _, tc := range []struct {
		status int
		body   string
		want   User
	}{
		{200, `{"id":"104","email":"a@example.com","name":"Alice","picture":"https://example.com/p"}`, User{ID: "104", Email: "a@example.com", Name: "Alice"}},
		{200, `{"id":"104","email":"a@example.com"}`, User{ID: "104", Email: "a@example.com"}},
		{200, `{"email":"a@example.com"}`, User{}},
		{200, `{"id":"104"}`, User{}},
		{200, `not json`, User{}},
		{401, `{"error":"invalid_token"}`, User{}},
	} {
		client := &http.Client{Transport: userTransport(func(r *http.Request) (*http.Response, error) {
			if r.URL.String() != UserInfoURL || r.Header.Get("Authorization") != "Bearer access" {
				t.Errorf("unexpected request: %s", r.URL)
			}
			return &http.Response{StatusCode: tc.status, Body: io.NopCloser(strings.NewReader(tc.body)), Request: r}, nil
		})}
		user, err := FetchUser(context.Background(), client, "access")
		if (err == nil) != (tc.want.ID != "") || user != tc.want {
			t.Fatalf("%s: user=%+v err=%v", tc.body, user, err)
		}
		if err != nil && strings.Contains(err.Error(), "access") {
			t.Fatal("error echoes the access token")
		}
	}
}
