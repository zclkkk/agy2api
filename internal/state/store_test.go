package state

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestPersistenceAndModelBoundReplay(t *testing.T) {
	dir := t.TempDir()
	s, err := Open(dir, time.Hour, 10)
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
	s, err = Open(dir, time.Hour, 10)
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
	s, err := Open(t.TempDir(), time.Hour, 2)
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
	s, err := Open(t.TempDir(), time.Hour, 10)
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
