package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Вход только через Google. password_hash остался у аккаунтов, заведённых
// до этого по почте и паролю: такой аккаунт забирает себе первый вход через
// Google с той же почтой (см. GoogleUser).
const sessionTTL = 30 * 24 * time.Hour

type User struct {
	ID    int64  `json:"id"`
	Email string `json:"email"`
	Name  string `json:"name"`
}

func normalizeEmail(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

func randomToken() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b) // never fails since Go 1.24
	return base64.RawURLEncoding.EncodeToString(b)
}

func hashToken(token string) []byte {
	h := sha256.Sum256([]byte(token))
	return h[:]
}

// createUser adds a Google user with the starting categories.
func (s *Store) createUser(ctx context.Context, email, name, googleSub string) (*User, error) {
	if name == "" {
		name = email[:strings.Index(email, "@")]
	}
	u := &User{Email: email, Name: name}
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `
			INSERT INTO users (email, name, google_sub) VALUES ($1, $2, $3) RETURNING id`,
			email, name, googleSub).Scan(&u.ID)
		if err != nil {
			return err
		}
		return seedCategories(ctx, tx, u.ID)
	})
	var pe *pgconn.PgError
	if errors.As(err, &pe) && pe.Code == "23505" {
		return nil, &APIError{http.StatusConflict, "Эта почта уже привязана к другому Google-аккаунту"}
	}
	if err != nil {
		return nil, err
	}
	return u, nil
}

// GoogleUser finds the user signing in with Google: by the Google account, then
// by email, or creates one. The email must be verified by Google.
func (s *Store) GoogleUser(ctx context.Context, sub, email, name string) (*User, error) {
	u := &User{}
	err := s.db.QueryRow(ctx, `SELECT id, email, name FROM users WHERE google_sub = $1`, sub).Scan(&u.ID, &u.Email, &u.Name)
	if err == nil {
		return u, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}

	// An account left from password sign-in with this email is taken over by
	// the Google account. Sign-up never verified emails, so anyone could have
	// registered the address first: the password is dropped and its sessions
	// end, leaving access only to the owner of the address, whom Google verified.
	email = normalizeEmail(email)
	err = pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `
			UPDATE users SET google_sub = $2, password_hash = NULL
			WHERE email = $1 AND google_sub IS NULL RETURNING id, email, name`, email, sub).
			Scan(&u.ID, &u.Email, &u.Name)
		if err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `DELETE FROM sessions WHERE user_id = $1`, u.ID)
		return err
	})
	if err == nil {
		return u, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	return s.createUser(ctx, email, strings.TrimSpace(name), sub)
}

// CreateSession returns a new session token; only its hash is stored.
func (s *Store) CreateSession(ctx context.Context, uid int64) (string, error) {
	if _, err := s.db.Exec(ctx, `DELETE FROM sessions WHERE expires_at < now()`); err != nil {
		return "", err
	}
	token := randomToken()
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `INSERT INTO sessions (token_hash, user_id, expires_at) VALUES ($1, $2, $3)`,
			hashToken(token), uid, time.Now().Add(sessionTTL)); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE users SET last_login_at = now() WHERE id = $1`, uid)
		return err
	})
	return token, err
}

// UserInfo is a user as the admin listing shows them. Password hashes stay out.
type UserInfo struct {
	User
	Password    bool
	Google      bool
	Accounts    int
	Txs         int
	CreatedAt   time.Time
	LastLoginAt *time.Time
}

func (s *Store) ListUsers(ctx context.Context) ([]UserInfo, error) {
	rows, _ := s.db.Query(ctx, `
		SELECT u.id, u.email, u.name, u.password_hash IS NOT NULL, u.google_sub IS NOT NULL,
		       (SELECT count(*) FROM accounts WHERE user_id = u.id),
		       (SELECT count(*) FROM transactions WHERE user_id = u.id),
		       u.created_at, u.last_login_at
		FROM users u ORDER BY u.id`)
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (UserInfo, error) {
		var u UserInfo
		err := r.Scan(&u.ID, &u.Email, &u.Name, &u.Password, &u.Google, &u.Accounts, &u.Txs, &u.CreatedAt, &u.LastLoginAt)
		return u, err
	})
}

// SessionUser returns the user of a live session, or nil.
func (s *Store) SessionUser(ctx context.Context, token string) (*User, error) {
	u := &User{}
	err := s.db.QueryRow(ctx, `
		SELECT u.id, u.email, u.name FROM sessions s JOIN users u ON u.id = s.user_id
		WHERE s.token_hash = $1 AND s.expires_at > now()`, hashToken(token)).
		Scan(&u.ID, &u.Email, &u.Name)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return u, nil
}

func (s *Store) DeleteSession(ctx context.Context, token string) error {
	_, err := s.db.Exec(ctx, `DELETE FROM sessions WHERE token_hash = $1`, hashToken(token))
	return err
}
