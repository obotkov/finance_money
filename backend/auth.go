package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"time"

	"golang.org/x/oauth2"
)

const (
	sessionCookie        = "session"
	googleStateCookie    = "google_state"
	googleVerifierCookie = "google_verifier"
	googleCookiePath     = "/api/auth/google"
	googleUserinfoURL    = "https://openidconnect.googleapis.com/v1/userinfo"
)

func (a *API) authConfig(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]bool{"google": a.google != nil})
}

func (a *API) me(w http.ResponseWriter, r *http.Request) {
	if u, ok := a.requireUser(w, r); ok {
		writeJSON(w, http.StatusOK, u)
	}
}

// signIn starts a session in a cookie and answers with the user.
func (a *API) signIn(w http.ResponseWriter, r *http.Request, u *User) {
	token, err := a.store.CreateSession(r.Context(), u.ID)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	a.setCookie(w, sessionCookie, token, "/", sessionTTL)
	writeJSON(w, http.StatusOK, u)
}

func (a *API) logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		if err := a.store.DeleteSession(r.Context(), c.Value); err != nil {
			a.fail(w, r, err)
			return
		}
	}
	a.setCookie(w, sessionCookie, "", "/", -1)
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// currentUser is the user of the request's session cookie, or nil.
func (a *API) currentUser(r *http.Request) (*User, error) {
	c, err := r.Cookie(sessionCookie)
	if err != nil || c.Value == "" {
		return nil, nil
	}
	return a.store.SessionUser(r.Context(), c.Value)
}

// requireUser returns the signed-in user or answers 401.
func (a *API) requireUser(w http.ResponseWriter, r *http.Request) (*User, bool) {
	u, err := a.currentUser(r)
	if err != nil {
		a.fail(w, r, err)
		return nil, false
	}
	if u == nil {
		writeJSON(w, http.StatusUnauthorized, errorBody("Войдите в аккаунт"))
		return nil, false
	}
	return u, true
}

// setCookie sets an HttpOnly cookie; a negative ttl deletes it.
func (a *API) setCookie(w http.ResponseWriter, name, value, path string, ttl time.Duration) {
	c := &http.Cookie{Name: name, Value: value, Path: path, HttpOnly: true, Secure: a.secure, SameSite: http.SameSiteLaxMode}
	if ttl < 0 {
		c.MaxAge = -1
	} else {
		c.MaxAge = int(ttl.Seconds())
	}
	http.SetCookie(w, c)
}

// googleStart sends the browser to Google's consent screen, with PKCE.
func (a *API) googleStart(w http.ResponseWriter, r *http.Request) {
	if a.google == nil {
		http.NotFound(w, r)
		return
	}
	state, verifier := randomToken(), oauth2.GenerateVerifier()
	a.setCookie(w, googleStateCookie, state, googleCookiePath, 10*time.Minute)
	a.setCookie(w, googleVerifierCookie, verifier, googleCookiePath, 10*time.Minute)
	http.Redirect(w, r, a.google.AuthCodeURL(state,
		oauth2.S256ChallengeOption(verifier),
		oauth2.SetAuthURLParam("prompt", "select_account")), http.StatusFound)
}

// googleCallback finishes the Google sign-in and returns to the page;
// a failure comes back to it as ?auth_error=<message>.
func (a *API) googleCallback(w http.ResponseWriter, r *http.Request) {
	if a.google == nil {
		http.NotFound(w, r)
		return
	}
	back := func(msg string, err error) {
		if err != nil {
			a.log.Warn("google sign-in failed", "err", err)
		}
		http.Redirect(w, r, "/?auth_error="+url.QueryEscape(msg), http.StatusFound)
	}

	state, _ := r.Cookie(googleStateCookie)
	verifier, _ := r.Cookie(googleVerifierCookie)
	a.setCookie(w, googleStateCookie, "", googleCookiePath, -1)
	a.setCookie(w, googleVerifierCookie, "", googleCookiePath, -1)
	q := r.URL.Query()
	if q.Get("error") != "" {
		back("Вход через Google отменён", nil)
		return
	}
	if state == nil || verifier == nil || subtle.ConstantTimeCompare([]byte(q.Get("state")), []byte(state.Value)) != 1 {
		back("Вход устарел — попробуйте ещё раз", nil)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	tok, err := a.google.Exchange(ctx, q.Get("code"), oauth2.VerifierOption(verifier.Value))
	if err != nil {
		back("Google не подтвердил вход", err)
		return
	}
	p, err := fetchGoogleProfile(ctx, a.google.Client(ctx, tok))
	if err != nil {
		back("Не удалось получить профиль Google", err)
		return
	}
	if p.Email == "" || !p.EmailVerified {
		back("У аккаунта Google нет подтверждённой почты", nil)
		return
	}
	u, err := a.store.GoogleUser(ctx, p.Sub, p.Email, p.Name)
	if err != nil {
		var ae *APIError
		if errors.As(err, &ae) {
			back(ae.Msg, nil)
		} else {
			back("Не удалось войти", err)
		}
		return
	}
	token, err := a.store.CreateSession(ctx, u.ID)
	if err != nil {
		back("Не удалось войти", err)
		return
	}
	a.setCookie(w, sessionCookie, token, "/", sessionTTL)
	http.Redirect(w, r, "/", http.StatusFound)
}

type googleProfile struct {
	Sub           string `json:"sub"`
	Email         string `json:"email"`
	EmailVerified bool   `json:"email_verified"`
	Name          string `json:"name"`
}

func fetchGoogleProfile(ctx context.Context, client *http.Client) (*googleProfile, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, googleUserinfoURL, nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("userinfo: %s", resp.Status)
	}
	var p googleProfile
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&p); err != nil {
		return nil, err
	}
	if p.Sub == "" {
		return nil, errors.New("userinfo: no sub")
	}
	return &p, nil
}
