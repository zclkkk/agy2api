// Package state stores bounded, expiring protocol state in a local SQLite file.
package state

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

var ErrNotFound = errors.New("state not found or expired")

type Store struct {
	db       *sql.DB
	aead     cipher.AEAD
	ttl      time.Duration
	capacity int
}

func Open(dir string, ttl time.Duration, capacity int) (*Store, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, "state.db")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	_ = f.Close()
	if err := os.Chmod(path, 0600); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	fail := func(err error) (*Store, error) { db.Close(); return nil, err }
	if _, err := db.Exec(`PRAGMA busy_timeout=5000; PRAGMA journal_mode=DELETE;
CREATE TABLE IF NOT EXISTS records(kind TEXT NOT NULL,id TEXT NOT NULL,data BLOB NOT NULL,expires INTEGER NOT NULL,created INTEGER NOT NULL,PRIMARY KEY(kind,id));
CREATE INDEX IF NOT EXISTS records_expiry ON records(expires);
CREATE TABLE IF NOT EXISTS metadata(key TEXT PRIMARY KEY,value BLOB NOT NULL);`); err != nil {
		return fail(err)
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return fail(err)
	}
	if _, err := db.Exec(`INSERT OR IGNORE INTO metadata(key,value) VALUES('replay_key',?)`, key); err != nil {
		return fail(err)
	}
	if err := db.QueryRow(`SELECT value FROM metadata WHERE key='replay_key'`).Scan(&key); err != nil {
		return fail(err)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return fail(err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return fail(err)
	}
	s := &Store{db: db, aead: aead, ttl: ttl, capacity: capacity}
	if err := s.Prune(context.Background()); err != nil {
		return fail(err)
	}
	return s, nil
}

func (s *Store) Close() error { return s.db.Close() }

// BindAccount prevents old signed context from being reused with a different
// Antigravity project after credentials are replaced.
func (s *Store) BindAccount(ctx context.Context, project string) error {
	fingerprint := sha256.Sum256([]byte(project))
	if _, err := s.db.ExecContext(ctx, `INSERT OR IGNORE INTO metadata(key,value) VALUES('account',?)`, fingerprint[:]); err != nil {
		return err
	}
	var existing []byte
	if err := s.db.QueryRowContext(ctx, `SELECT value FROM metadata WHERE key='account'`).Scan(&existing); err != nil {
		return err
	}
	if string(existing) != string(fingerprint[:]) {
		return errors.New("data directory belongs to another Antigravity account; use a different data_dir")
	}
	return nil
}
func (s *Store) Prune(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM records WHERE expires<=?`, time.Now().Unix())
	return err
}

func (s *Store) Put(ctx context.Context, kind, id string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if len(data) > 64<<20 {
		return errors.New("protocol state exceeds 64 MiB")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now()
	if _, err = tx.ExecContext(ctx, `DELETE FROM records WHERE expires<=?`, now.Unix()); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT OR REPLACE INTO records(kind,id,data,expires,created) VALUES(?,?,?,?,?)`, kind, id, data, now.Add(s.ttl).Unix(), now.UnixNano()); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM records WHERE rowid IN (SELECT rowid FROM records ORDER BY created DESC LIMIT -1 OFFSET ?)`, s.capacity); err != nil {
		return err
	}
	return tx.Commit()
}

func (s *Store) Get(ctx context.Context, kind, id string, value any) error {
	var data []byte
	err := s.db.QueryRowContext(ctx, `SELECT data FROM records WHERE kind=? AND id=? AND expires>?`, kind, id, time.Now().Unix()).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return err
	}
	return json.Unmarshal(data, value)
}
func (s *Store) Delete(ctx context.Context, kind, id string) (bool, error) {
	r, err := s.db.ExecContext(ctx, `DELETE FROM records WHERE kind=? AND id=? AND expires>?`, kind, id, time.Now().Unix())
	if err != nil {
		return false, err
	}
	n, err := r.RowsAffected()
	return n > 0, err
}

// Seal creates an authenticated, client-carried replay item. It does not write a record.
func (s *Store) Seal(model string, value any) (string, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	sealed := s.aead.Seal(nonce, nonce, data, []byte(model))
	return "agy1." + base64.RawURLEncoding.EncodeToString(sealed), nil
}
func (s *Store) Unseal(model, token string, value any) error {
	if !strings.HasPrefix(token, "agy1.") {
		return errors.New("reasoning item was not issued by this agy2api instance")
	}
	data, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(token, "agy1."))
	if err != nil || len(data) < s.aead.NonceSize() {
		return errors.New("invalid reasoning item")
	}
	data, err = s.aead.Open(nil, data[:s.aead.NonceSize()], data[s.aead.NonceSize():], []byte(model))
	if err != nil {
		return errors.New("reasoning item belongs to another model or instance, or was modified")
	}
	if err := json.Unmarshal(data, value); err != nil {
		return fmt.Errorf("decode reasoning item: %w", err)
	}
	return nil
}
