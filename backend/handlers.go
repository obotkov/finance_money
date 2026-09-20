package main

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
)

type Config struct {
	PublicURL          string // the site as the browser sees it, e.g. https://okayconnect.online
	GoogleClientID     string // Google sign-in is off while these are empty
	GoogleClientSecret string
}

type API struct {
	store   *Store
	rates   *RateUpdater
	log     *slog.Logger
	origin  string         // the site's own origin: writes from other origins are refused
	secure  bool           // cookies only over HTTPS
	google  *oauth2.Config // nil when Google sign-in isn't configured
	limiter *limiter
}

func NewAPI(store *Store, rates *RateUpdater, cfg Config, log *slog.Logger) *API {
	a := &API{
		store:   store,
		rates:   rates,
		log:     log,
		origin:  cfg.PublicURL,
		secure:  strings.HasPrefix(cfg.PublicURL, "https://"),
		limiter: newLimiter(15 * time.Minute),
	}
	if cfg.GoogleClientID != "" && cfg.GoogleClientSecret != "" {
		a.google = &oauth2.Config{
			ClientID:     cfg.GoogleClientID,
			ClientSecret: cfg.GoogleClientSecret,
			Endpoint:     google.Endpoint,
			RedirectURL:  cfg.PublicURL + "/api/auth/google/callback",
			Scopes:       []string{"openid", "email", "profile"},
		}
	}
	return a
}

// Handler serves the JSON API. Every data write answers with the user's full
// state, which the page swaps in whole — the data of one person is small.
func (a *API) Handler() http.Handler {
	s := a.store
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/health", a.health)

	mux.HandleFunc("GET /api/auth/config", a.authConfig)
	mux.HandleFunc("GET /api/auth/me", a.me)
	mux.HandleFunc("POST /api/auth/register", a.register)
	mux.HandleFunc("POST /api/auth/login", a.login)
	mux.HandleFunc("POST /api/auth/logout", a.logout)
	mux.HandleFunc("GET /api/auth/google/start", a.googleStart)
	mux.HandleFunc("GET /api/auth/google/callback", a.googleCallback)

	mux.HandleFunc("GET /api/state", a.withState(func(*http.Request, int64) error { return nil }))

	mux.HandleFunc("POST /api/accounts", a.withState(withBody(s.CreateAccount)))
	mux.HandleFunc("PUT /api/accounts/{id}", a.withState(withIDBody(s.UpdateAccount)))
	mux.HandleFunc("DELETE /api/accounts/{id}", a.withState(withID(s.DeleteAccount)))

	mux.HandleFunc("POST /api/transactions", a.withState(withBody(s.CreateTx)))
	mux.HandleFunc("PUT /api/transactions/{id}", a.withState(withIDBody(s.UpdateTx)))
	mux.HandleFunc("DELETE /api/transactions/{id}", a.withState(withID(s.DeleteTx)))

	mux.HandleFunc("POST /api/categories", a.withState(withBody(s.CreateCategory)))
	mux.HandleFunc("PUT /api/categories/{id}", a.withState(withIDBody(s.UpdateCategory)))
	mux.HandleFunc("DELETE /api/categories/{id}", a.withState(withID(s.DeleteCategory)))

	mux.HandleFunc("POST /api/crypto/portfolios", a.withState(withBody(s.CreateCryptoPortfolio)))
	mux.HandleFunc("PUT /api/crypto/portfolios/{id}", a.withState(withIDBody(s.UpdateCryptoPortfolio)))
	mux.HandleFunc("DELETE /api/crypto/portfolios/{id}", a.withState(withID(s.DeleteCryptoPortfolio)))

	mux.HandleFunc("POST /api/crypto/assets", a.withState(withBody(s.CreateCryptoAsset)))
	mux.HandleFunc("PUT /api/crypto/assets/{id}", a.withState(withIDBody(s.UpdateCryptoAsset)))
	mux.HandleFunc("DELETE /api/crypto/assets/{id}", a.withState(withID(s.DeleteCryptoAsset)))

	mux.HandleFunc("POST /api/rates/refresh", a.withState(func(r *http.Request, _ int64) error {
		if err := a.rates.Refresh(r.Context()); err != nil {
			return &APIError{http.StatusBadGateway, "Не удалось обновить курсы: " + err.Error()}
		}
		return nil
	}))
	mux.HandleFunc("POST /api/data/reset", a.withState(func(r *http.Request, uid int64) error {
		return s.Reset(r.Context(), uid)
	}))
	mux.HandleFunc("POST /api/import", a.importCSV)
	return a.middleware(mux)
}

func (a *API) health(w http.ResponseWriter, r *http.Request) {
	if err := a.store.Ping(r.Context()); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, errorBody("database unavailable"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// withState runs fn for the signed-in user and answers with their state.
func (a *API) withState(fn func(r *http.Request, uid int64) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u, ok := a.requireUser(w, r)
		if !ok {
			return
		}
		if err := fn(r, u.ID); err != nil {
			a.fail(w, r, err)
			return
		}
		st, err := a.state(r.Context(), u)
		if err != nil {
			a.fail(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, st)
	}
}

func (a *API) state(ctx context.Context, u *User) (*State, error) {
	st, err := a.store.State(ctx, u.ID)
	if err != nil {
		return nil, err
	}
	st.User = u
	return st, nil
}

func (a *API) importCSV(w http.ResponseWriter, r *http.Request) {
	u, ok := a.requireUser(w, r)
	if !ok {
		return
	}
	var in struct {
		Text string `json:"text"`
	}
	if err := decode(r, &in); err != nil {
		a.fail(w, r, err)
		return
	}
	n, err := a.store.Import(r.Context(), u.ID, in.Text)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	st, err := a.state(r.Context(), u)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		*State
		Imported int `json:"imported"`
	}{st, n})
}

func withBody[T any](fn func(context.Context, int64, T) error) func(*http.Request, int64) error {
	return func(r *http.Request, uid int64) error {
		var in T
		if err := decode(r, &in); err != nil {
			return err
		}
		return fn(r.Context(), uid, in)
	}
}

func withID(fn func(context.Context, int64, int64) error) func(*http.Request, int64) error {
	return func(r *http.Request, uid int64) error {
		id, err := pathID(r)
		if err != nil {
			return err
		}
		return fn(r.Context(), uid, id)
	}
}

func withIDBody[T any](fn func(context.Context, int64, int64, T) error) func(*http.Request, int64) error {
	return func(r *http.Request, uid int64) error {
		id, err := pathID(r)
		if err != nil {
			return err
		}
		var in T
		if err := decode(r, &in); err != nil {
			return err
		}
		return fn(r.Context(), uid, id, in)
	}
}

func pathID(r *http.Request) (int64, error) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		return 0, notFound("Не найдено")
	}
	return id, nil
}

func decode(r *http.Request, v any) error {
	if err := json.NewDecoder(r.Body).Decode(v); err != nil {
		return badRequest("Некорректный запрос: " + err.Error())
	}
	return nil
}

func (a *API) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		defer func() {
			if p := recover(); p != nil {
				a.log.Error("panic", "method", r.Method, "path", r.URL.Path, "panic", p)
				writeJSON(w, http.StatusInternalServerError, errorBody("Внутренняя ошибка сервера"))
			}
		}()
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			// The session cookie is SameSite=Lax and writes must be JSON, which
			// another site can't send without a CORS preflight that is never
			// granted. Checking Origin on top closes the rest.
			if o := r.Header.Get("Origin"); o != "" && o != a.origin {
				writeJSON(w, http.StatusForbidden, errorBody("Запрос с чужого сайта"))
				return
			}
			if r.Method != http.MethodDelete && !strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
				writeJSON(w, http.StatusUnsupportedMediaType, errorBody("Ожидается JSON"))
				return
			}
		}
		r.Body = http.MaxBytesReader(w, r.Body, 5<<20)
		next.ServeHTTP(w, r)
		if r.Method != http.MethodGet {
			a.log.Info("request", "method", r.Method, "path", r.URL.Path, "took", time.Since(start).Round(time.Millisecond))
		}
	})
}

func (a *API) fail(w http.ResponseWriter, r *http.Request, err error) {
	var ae *APIError
	var pe *pgconn.PgError
	switch {
	case errors.As(err, &ae):
		writeJSON(w, ae.Status, errorBody(ae.Msg))
	case errors.As(err, &pe) && pe.Code == "23505": // unique_violation
		writeJSON(w, http.StatusConflict, errorBody("Такое название уже есть"))
	default:
		a.log.Error("request failed", "method", r.Method, "path", r.URL.Path, "err", err)
		writeJSON(w, http.StatusInternalServerError, errorBody("Внутренняя ошибка сервера"))
	}
}

func errorBody(msg string) map[string]string { return map[string]string{"error": msg} }

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
