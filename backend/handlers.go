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
)

type API struct {
	store *Store
	rates *RateUpdater
	log   *slog.Logger
}

func NewAPI(store *Store, rates *RateUpdater, log *slog.Logger) *API {
	return &API{store: store, rates: rates, log: log}
}

// Handler serves the JSON API. Every write answers with the full state, which
// the page swaps in whole — there is one user and the data is small.
func (a *API) Handler() http.Handler {
	s := a.store
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/health", a.health)
	mux.HandleFunc("GET /api/state", a.withState(func(*http.Request) error { return nil }))

	mux.HandleFunc("POST /api/accounts", a.withState(withBody(s.CreateAccount)))
	mux.HandleFunc("PUT /api/accounts/{id}", a.withState(withIDBody(s.UpdateAccount)))
	mux.HandleFunc("DELETE /api/accounts/{id}", a.withState(withID(s.DeleteAccount)))

	mux.HandleFunc("POST /api/transactions", a.withState(withBody(s.CreateTx)))
	mux.HandleFunc("DELETE /api/transactions/{id}", a.withState(withID(s.DeleteTx)))

	mux.HandleFunc("POST /api/categories", a.withState(withBody(s.CreateCategory)))
	mux.HandleFunc("PUT /api/categories/{id}", a.withState(withIDBody(s.UpdateCategory)))
	mux.HandleFunc("DELETE /api/categories/{id}", a.withState(withID(s.DeleteCategory)))

	mux.HandleFunc("POST /api/rates/refresh", a.withState(func(r *http.Request) error {
		if err := a.rates.Refresh(r.Context()); err != nil {
			return &APIError{http.StatusBadGateway, "Не удалось обновить курсы: " + err.Error()}
		}
		return nil
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

// withState runs fn and answers with the full state.
func (a *API) withState(fn func(*http.Request) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := fn(r); err != nil {
			a.fail(w, r, err)
			return
		}
		st, err := a.store.State(r.Context())
		if err != nil {
			a.fail(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, st)
	}
}

func (a *API) importCSV(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Text string `json:"text"`
	}
	if err := decode(r, &in); err != nil {
		a.fail(w, r, err)
		return
	}
	n, err := a.store.Import(r.Context(), in.Text)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	st, err := a.store.State(r.Context())
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, struct {
		*State
		Imported int `json:"imported"`
	}{st, n})
}

func withBody[T any](fn func(context.Context, T) error) func(*http.Request) error {
	return func(r *http.Request) error {
		var in T
		if err := decode(r, &in); err != nil {
			return err
		}
		return fn(r.Context(), in)
	}
}

func withID(fn func(context.Context, int64) error) func(*http.Request) error {
	return func(r *http.Request) error {
		id, err := pathID(r)
		if err != nil {
			return err
		}
		return fn(r.Context(), id)
	}
}

func withIDBody[T any](fn func(context.Context, int64, T) error) func(*http.Request) error {
	return func(r *http.Request) error {
		id, err := pathID(r)
		if err != nil {
			return err
		}
		var in T
		if err := decode(r, &in); err != nil {
			return err
		}
		return fn(r.Context(), id, in)
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
		// Writes must be JSON: another site can't send that content type without
		// a CORS preflight, which is never granted, so it can't ride on the
		// browser's saved basic-auth credentials.
		if (r.Method == http.MethodPost || r.Method == http.MethodPut) &&
			!strings.HasPrefix(r.Header.Get("Content-Type"), "application/json") {
			writeJSON(w, http.StatusUnsupportedMediaType, errorBody("Ожидается JSON"))
			return
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
