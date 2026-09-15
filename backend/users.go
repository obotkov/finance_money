package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/http"
	"net/mail"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"golang.org/x/crypto/bcrypt"
)

const (
	sessionTTL  = 30 * 24 * time.Hour
	bcryptCost  = 12
	minPassword = 8  // characters
	maxPassword = 72 // bytes: bcrypt ignores the rest
)

type User struct {
	ID    int64  `json:"id"`
	Email string `json:"email"`
	Name  string `json:"name"`
}

var errBadCredentials = &APIError{http.StatusUnauthorized, "Неверная почта или пароль"}

// dummyHash makes a sign-in with an unknown email as slow as one with a wrong
// password, so response time doesn't tell which emails are registered.
var dummyHash, _ = bcrypt.GenerateFromPassword([]byte("not a real password"), bcryptCost)

func normalizeEmail(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

// validateCredentials expects an already normalized email.
func validateCredentials(email, password string) error {
	addr, err := mail.ParseAddress(email)
	if err != nil || addr.Address != email || !strings.Contains(email[strings.LastIndex(email, "@"):], ".") {
		return badRequest("Проверьте адрес почты")
	}
	if utf8.RuneCountInString(password) < minPassword {
		return badRequest("Пароль — не короче 8 символов")
	}
	if len(password) > maxPassword {
		return badRequest("Пароль слишком длинный")
	}
	return nil
}

func randomToken() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b) // never fails since Go 1.24
	return base64.RawURLEncoding.EncodeToString(b)
}

func hashToken(token string) []byte {
	h := sha256.Sum256([]byte(token))
	return h[:]
}

func (s *Store) Register(ctx context.Context, email, password, name string) (*User, error) {
	email = normalizeEmail(email)
	if err := validateCredentials(email, password); err != nil {
		return nil, err
	}
	hash, err := bcrypt.GenerateFromPassword([]byte(password), bcryptCost)
	if err != nil {
		return nil, err
	}
	return s.createUser(ctx, email, strings.TrimSpace(name), string(hash), "")
}

// createUser adds a user with the starting categories. The password hash is
// empty for Google accounts, the Google sub for password accounts.
func (s *Store) createUser(ctx context.Context, email, name, hash, googleSub string) (*User, error) {
	if name == "" {
		name = email[:strings.Index(email, "@")]
	}
	u := &User{Email: email, Name: name}
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		err := tx.QueryRow(ctx, `
			INSERT INTO users (email, name, password_hash, google_sub)
			VALUES ($1, $2, NULLIF($3, ''), NULLIF($4, '')) RETURNING id`,
			email, name, hash, googleSub).Scan(&u.ID)
		if err != nil {
			return err
		}
		return seedCategories(ctx, tx, u.ID)
	})
	var pe *pgconn.PgError
	if errors.As(err, &pe) && pe.Code == "23505" {
		return nil, &APIError{http.StatusConflict, "Эта почта уже зарегистрирована — войдите"}
	}
	if err != nil {
		return nil, err
	}
	return u, nil
}

func (s *Store) Authenticate(ctx context.Context, email, password string) (*User, error) {
	u := &User{}
	var hash *string
	err := s.db.QueryRow(ctx, `SELECT id, email, name, password_hash FROM users WHERE email = $1`, normalizeEmail(email)).
		Scan(&u.ID, &u.Email, &u.Name, &hash)
	if errors.Is(err, pgx.ErrNoRows) {
		_ = bcrypt.CompareHashAndPassword(dummyHash, []byte(password))
		return nil, errBadCredentials
	}
	if err != nil {
		return nil, err
	}
	if hash == nil {
		return nil, &APIError{http.StatusUnauthorized, "Этот адрес привязан к Google — войдите через Google"}
	}
	if bcrypt.CompareHashAndPassword([]byte(*hash), []byte(password)) != nil {
		return nil, errBadCredentials
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

	// A password account with this email is taken over by the Google account.
	// Sign-up doesn't verify emails, so anyone could have registered the
	// address first: the password is dropped and its sessions end, leaving
	// access only to the owner of the address, whom Google has verified.
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
	return s.createUser(ctx, email, strings.TrimSpace(name), "", sub)
}

// CreateSession returns a new session token; only its hash is stored.
func (s *Store) CreateSession(ctx context.Context, uid int64) (string, error) {
	if _, err := s.db.Exec(ctx, `DELETE FROM sessions WHERE expires_at < now()`); err != nil {
		return "", err
	}
	token := randomToken()
	_, err := s.db.Exec(ctx, `INSERT INTO sessions (token_hash, user_id, expires_at) VALUES ($1, $2, $3)`,
		hashToken(token), uid, time.Now().Add(sessionTTL))
	return token, err
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
