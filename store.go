package main

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"embed"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrations embed.FS

type store struct {
	db   *pgxpool.Pool
	aead cipher.AEAD
}

func newStore(ctx context.Context, url string, key []byte) (*store, error) {
	if len(key) != 32 {
		return nil, errors.New("TOKEN_ENC_KEY 32 bayt (base64) olmali")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		return nil, err
	}
	cfg.MaxConns = 3
	var pool *pgxpool.Pool
	for i := 0; ; i++ {
		pool, err = pgxpool.NewWithConfig(ctx, cfg)
		if err == nil {
			if err = pool.Ping(ctx); err == nil {
				break
			}
			pool.Close()
		}
		if i >= 15 {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	s := &store{db: pool, aead: aead}
	return s, s.migrate(ctx)
}

func (s *store) migrate(ctx context.Context) error {
	files, err := migrations.ReadDir("migrations")
	if err != nil {
		return err
	}
	tx, err := s.db.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "SELECT pg_advisory_xact_lock(7342001)"); err != nil {
		return err
	}
	// Dosyalar ada gore sirali gelir; hepsi tekrar calistirilabilir (IF NOT EXISTS).
	for _, f := range files {
		sql, err := migrations.ReadFile("migrations/" + f.Name())
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, string(sql)); err != nil {
			return fmt.Errorf("migration %s: %w", f.Name(), err)
		}
	}
	return tx.Commit(ctx)
}

func (s *store) encrypt(plain string) ([]byte, error) {
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return s.aead.Seal(nonce, nonce, []byte(plain), []byte("refresh-token")), nil
}

func (s *store) decrypt(enc []byte) (string, error) {
	n := s.aead.NonceSize()
	if len(enc) < n {
		return "", errors.New("sifreli veri kisa")
	}
	plain, err := s.aead.Open(nil, enc[:n], enc[n:], []byte("refresh-token"))
	return string(plain), err
}

// loadRefresh returns "" if no token is stored.
func (s *store) loadRefresh(ctx context.Context) (token, scope string, err error) {
	var enc []byte
	err = s.db.QueryRow(ctx, "SELECT refresh_enc, scope FROM oauth_token WHERE id = 1").Scan(&enc, &scope)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", nil
	}
	if err != nil {
		return "", "", err
	}
	token, err = s.decrypt(enc)
	return token, scope, err
}

func (s *store) saveRefresh(ctx context.Context, token, scope string) error {
	enc, err := s.encrypt(token)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(ctx, `INSERT INTO oauth_token (id, refresh_enc, scope, updated_at) VALUES (1, $1, $2, now())
		ON CONFLICT (id) DO UPDATE SET refresh_enc = EXCLUDED.refresh_enc, scope = EXCLUDED.scope, updated_at = now()`, enc, scope)
	return err
}

func (s *store) getState(ctx context.Context, key string) (string, error) {
	var v string
	err := s.db.QueryRow(ctx, "SELECT value FROM state WHERE key = $1", key).Scan(&v)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return v, err
}

func (s *store) setState(ctx context.Context, key, value string) error {
	_, err := s.db.Exec(ctx, `INSERT INTO state (key, value) VALUES ($1, $2)
		ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value`, key, value)
	return err
}

func idHash(id string) []byte {
	h := sha256.Sum256([]byte(id))
	return h[:]
}

func (s *store) isProcessed(ctx context.Context, id string) (bool, error) {
	var ok bool
	err := s.db.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM processed WHERE id_hash = $1)", idHash(id)).Scan(&ok)
	return ok, err
}

// record: bir mail icin verilen karar. Ayrintilar (gonderen, konu, gerekce) 7 gun sonra silinir.
type record struct {
	ID         string
	Received   time.Time
	Level      level
	Notified   bool
	MarkedRead bool
	Sender     string
	Subject    string
	Reason     string
	Source     string
}

func (s *store) recordProcessed(ctx context.Context, r record) error {
	_, err := s.db.Exec(ctx, `INSERT INTO processed (id_hash, received_at, level, notified, marked_read, sender, subject, reason, source)
		VALUES ($1, $2, $3, $4, $5, NULLIF($6, ''), NULLIF($7, ''), NULLIF($8, ''), NULLIF($9, '')) ON CONFLICT (id_hash) DO NOTHING`,
		idHash(r.ID), r.Received, string(r.Level), r.Notified, r.MarkedRead, r.Sender, r.Subject, r.Reason, r.Source)
	return err
}

// forgetDetails: 7 gunden eski kararlarin gonderen/konu/gerekce alanlarini bosaltir.
func (s *store) forgetDetails(ctx context.Context, olderThan time.Duration) error {
	_, err := s.db.Exec(ctx, `UPDATE processed SET sender = NULL, subject = NULL, reason = NULL
		WHERE processed_at < $1 AND (sender IS NOT NULL OR subject IS NOT NULL OR reason IS NOT NULL)`, time.Now().Add(-olderThan))
	return err
}

type dayStats struct{ Total, Urgent, Important, MarkedRead int }

func (s *store) statsSince(ctx context.Context, since time.Time) (dayStats, error) {
	var d dayStats
	err := s.db.QueryRow(ctx, `SELECT count(*),
			count(*) FILTER (WHERE level = 'urgent'),
			count(*) FILTER (WHERE level = 'important'),
			count(*) FILTER (WHERE marked_read)
		FROM processed WHERE processed_at >= $1`, since).Scan(&d.Total, &d.Urgent, &d.Important, &d.MarkedRead)
	return d, err
}

func (s *store) purge(ctx context.Context, olderThan time.Duration) error {
	_, err := s.db.Exec(ctx, "DELETE FROM processed WHERE processed_at < $1", time.Now().Add(-olderThan))
	return err
}
