package state

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestPersistenceAndModelBoundReplay(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir, time.Hour, 10, 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := s.Put(ctx, "response", "id", map[string]string{"text": "persisted"}); err != nil {
		t.Fatal(err)
	}
	sealed, err := s.Seal("model-a", map[string]string{"signature": "opaque"})
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(dir, time.Hour, 10, 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var record map[string]string
	if err := s.Get(ctx, "response", "id", &record); err != nil || record["text"] != "persisted" {
		t.Fatalf("%v %v", record, err)
	}
	if err := s.Unseal("model-a", sealed, &record); err != nil || record["signature"] != "opaque" {
		t.Fatal(err)
	}
	if err := s.Unseal("model-b", sealed, &record); err == nil {
		t.Fatal("cross-model replay accepted")
	}
	if err := s.Unseal("model-a", sealed[:len(sealed)-2]+"xx", &record); err == nil {
		t.Fatal("tampered replay accepted")
	}
}

func TestCapacityExpirationAndDeletion(t *testing.T) {
	s, err := Open(t.TempDir(), time.Hour, 2, 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	for _, id := range []string{"a", "b", "c"} {
		if err := s.Put(ctx, "x", id, id); err != nil {
			t.Fatal(err)
		}
	}
	var value string
	if err := s.Get(ctx, "x", "a", &value); !errors.Is(err, ErrNotFound) {
		t.Fatal("oldest record was not evicted", err)
	}
	s.ttl = -time.Hour
	if err := s.Put(ctx, "x", "expired", "x"); err != nil {
		t.Fatal(err)
	}
	if err := s.Get(ctx, "x", "expired", &value); !errors.Is(err, ErrNotFound) {
		t.Fatal("expired record returned", err)
	}
	if ok, err := s.Delete(ctx, "x", "c"); !ok || err != nil {
		t.Fatal(ok, err)
	}
	if err := s.Get(ctx, "x", "c", &value); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
}

func TestAccountBinding(t *testing.T) {
	s, err := Open(t.TempDir(), time.Hour, 10, 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	if err := s.BindAccount(ctx, "project-a"); err != nil {
		t.Fatal(err)
	}
	if err := s.BindAccount(ctx, "project-a"); err != nil {
		t.Fatal(err)
	}
	if err := s.BindAccount(ctx, "project-b"); err == nil {
		t.Fatal("cross-account state reuse allowed")
	}
}

func TestSizeLimitEvictsOldestButKeepsNewest(t *testing.T) {
	s, err := Open(t.TempDir(), time.Hour, 100, 3000)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ctx := context.Background()
	big := strings.Repeat("x", 1000)
	for _, id := range []string{"a", "b", "c", "d"} {
		if err := s.Put(ctx, "x", id, big); err != nil {
			t.Fatal(err)
		}
	}
	var value string
	for id, want := range map[string]bool{"a": false, "b": false, "c": true, "d": true} {
		err := s.Get(ctx, "x", id, &value)
		if (err == nil) != want {
			t.Fatalf("record %s present=%v", id, err == nil)
		}
	}
	huge := strings.Repeat("y", 5000)
	if err := s.Put(ctx, "x", "huge", huge); err != nil {
		t.Fatal(err)
	}
	if err := s.Get(ctx, "x", "huge", &value); err != nil {
		t.Fatal("newest record was evicted", err)
	}
	if err := s.Get(ctx, "x", "d", &value); !errors.Is(err, ErrNotFound) {
		t.Fatal("older records were kept over the size limit")
	}
}

func TestLegacyTableIsMigrated(t *testing.T) {
	dir := t.TempDir()
	db, err := sql.Open("sqlite", filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE records(kind TEXT NOT NULL,id TEXT NOT NULL,data BLOB NOT NULL,expires INTEGER NOT NULL,created INTEGER NOT NULL,PRIMARY KEY(kind,id));
INSERT INTO records VALUES('tool','call_agy_1','"kept"',?,1)`, time.Now().Add(time.Hour).Unix()); err != nil {
		t.Fatal(err)
	}
	db.Close()
	s, err := Open(dir, time.Hour, 10, 1<<30)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var value string
	if err := s.Get(context.Background(), "tool", "call_agy_1", &value); err != nil || value != "kept" {
		t.Fatalf("legacy record lost: %q %v", value, err)
	}
}
