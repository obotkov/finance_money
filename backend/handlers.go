package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	PublicURL          string // the site as the browser sees it, e.g. https://penny.place
	GoogleClientID     string // Google sign-in is off while these are empty
	GoogleClientSecret string
}

type API struct {
	store  *Store
	rates  *RateUpdater
	push   *Pusher   // nil — пуши не настроены (в тестах)
	tg     *Telegram // nil — Telegram-бот не настроен
	log    *slog.Logger
	origin string         // the site's own origin: writes from other origins are refused
	secure bool           // cookies only over HTTPS
	google *oauth2.Config // nil when Google sign-in isn't configured
}

func NewAPI(store *Store, rates *RateUpdater, push *Pusher, cfg Config, log *slog.Logger) *API {
	a := &API{
		store:  store,
		rates:  rates,
		push:   push,
		log:    log,
		origin: cfg.PublicURL,
		secure: strings.HasPrefix(cfg.PublicURL, "https://"),
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
	mux.HandleFunc("POST /api/auth/logout", a.logout)
	mux.HandleFunc("GET /api/auth/google/start", a.googleStart)
	mux.HandleFunc("GET /api/auth/google/callback", a.googleCallback)

	mux.HandleFunc("GET /api/state", a.withState(func(*http.Request, int64) error { return nil }))

	mux.HandleFunc("POST /api/accounts", a.withData(withBody(s.CreateAccount)))
	mux.HandleFunc("PUT /api/accounts/{id}", a.withData(withIDBody(s.UpdateAccount)))
	// ?replace=<id> переносит операции на другой счёт вместо того, чтобы удалить их
	mux.HandleFunc("DELETE /api/accounts/{id}", a.withData(func(r *http.Request, uid int64) error {
		id, err := pathID(r)
		if err != nil {
			return err
		}
		var replace int64
		if v := r.URL.Query().Get("replace"); v != "" {
			if replace, err = strconv.ParseInt(v, 10, 64); err != nil || replace <= 0 {
				return badRequest("Счёт для переноса операций не найден")
			}
		}
		return s.DeleteAccount(r.Context(), uid, id, replace)
	}))

	mux.HandleFunc("POST /api/transactions", a.withData(withBody(s.CreateTx)))
	mux.HandleFunc("PUT /api/transactions/{id}", a.withData(withIDBody(s.UpdateTx)))
	mux.HandleFunc("DELETE /api/transactions/{id}", a.withData(withID(s.DeleteTx)))

	mux.HandleFunc("POST /api/categories", a.withData(withBody(s.CreateCategory)))
	mux.HandleFunc("PUT /api/categories/{id}", a.withData(withIDBody(s.UpdateCategory)))
	mux.HandleFunc("DELETE /api/categories/{id}", a.withData(withID(s.DeleteCategory)))

	mux.HandleFunc("POST /api/crypto/portfolios", a.withData(withBody(s.CreateCryptoPortfolio)))
	mux.HandleFunc("PUT /api/crypto/portfolios/{id}", a.withData(withIDBody(s.UpdateCryptoPortfolio)))
	mux.HandleFunc("DELETE /api/crypto/portfolios/{id}", a.withData(withID(s.DeleteCryptoPortfolio)))

	mux.HandleFunc("POST /api/crypto/assets", a.withData(withBody(s.CreateCryptoAsset)))
	mux.HandleFunc("PUT /api/crypto/assets/{id}", a.withData(withIDBody(s.UpdateCryptoAsset)))
	mux.HandleFunc("DELETE /api/crypto/assets/{id}", a.withData(withID(s.DeleteCryptoAsset)))

	// пулы ликвидности в крипто-портфеле: позиция с первым внесением, события, удаление
	mux.HandleFunc("POST /api/pools", a.withData(withBody(s.CreatePool)))
	mux.HandleFunc("PUT /api/pools/{id}", a.withData(withIDBody(s.UpdatePool)))
	mux.HandleFunc("DELETE /api/pools/{id}", a.withData(withID(s.DeletePool)))
	mux.HandleFunc("POST /api/pools/{id}/events", a.withData(withIDBody(s.AddPoolEvent)))
	mux.HandleFunc("DELETE /api/pools/{id}/events/{event}", a.withData(func(r *http.Request, uid int64) error {
		id, err := pathID(r)
		if err != nil {
			return err
		}
		event, err := strconv.ParseInt(r.PathValue("event"), 10, 64)
		if err != nil {
			return notFound("Не найдено")
		}
		return s.DeletePoolEvent(r.Context(), uid, id, event)
	}))

	mux.HandleFunc("POST /api/recurring", a.withData(withBody(s.CreateRecurring)))
	mux.HandleFunc("PUT /api/recurring/{id}", a.withData(withIDBody(s.UpdateRecurring)))
	mux.HandleFunc("DELETE /api/recurring/{id}", a.withData(withID(s.DeleteRecurring)))

	mux.HandleFunc("POST /api/budgets", a.withData(withBody(s.CreateBudget)))
	mux.HandleFunc("PUT /api/budgets/{id}", a.withData(withIDBody(s.UpdateBudget)))
	mux.HandleFunc("DELETE /api/budgets/{id}", a.withData(withID(s.DeleteBudget)))
	mux.HandleFunc("PUT /api/budgets/alert", a.withState(func(r *http.Request, uid int64) error {
		var in BudgetAlert
		if err := decode(r, &in); err != nil {
			return err
		}
		if err := s.SetBudgetAlert(r.Context(), uid, in); err != nil {
			return err
		}
		a.checkBudgets(uid)
		return nil
	}))

	mux.HandleFunc("PUT /api/widgets", a.withState(withBody(s.SetWidgets)))

	mux.HandleFunc("POST /api/push/subscribe", a.withState(withBody(s.SavePushSubscription)))
	mux.HandleFunc("POST /api/push/unsubscribe", a.withState(func(r *http.Request, uid int64) error {
		var in PushSubscription
		if err := decode(r, &in); err != nil {
			return err
		}
		return s.DeletePushSubscription(r.Context(), uid, in.Endpoint)
	}))
	mux.HandleFunc("POST /api/push/test", a.pushTest)

	// свои монеты: поиск в CoinGecko, добавление в список и удаление из него
	mux.HandleFunc("GET /api/coins/search", a.searchCoins)
	mux.HandleFunc("POST /api/coins", a.withState(withBody(a.AddCoin)))
	mux.HandleFunc("DELETE /api/coins/{code}", a.withState(func(r *http.Request, uid int64) error {
		return s.RemoveCoin(r.Context(), uid, r.PathValue("code"))
	}))

	// Telegram: ссылка для привязки чата, недельная рассылка, отправка сейчас, отключение
	mux.HandleFunc("POST /api/telegram/link", a.telegramLink)
	mux.HandleFunc("PUT /api/telegram", a.withState(func(r *http.Request, uid int64) error {
		var in struct {
			Weekly bool `json:"weekly"`
		}
		if err := decode(r, &in); err != nil {
			return err
		}
		return s.SetTelegramWeekly(r.Context(), uid, in.Weekly)
	}))
	mux.HandleFunc("DELETE /api/telegram", a.withState(func(r *http.Request, uid int64) error {
		return s.UnlinkTelegram(r.Context(), uid)
	}))
	mux.HandleFunc("POST /api/telegram/send", a.withState(func(r *http.Request, uid int64) error {
		if a.tg == nil {
			return &APIError{http.StatusServiceUnavailable, "Telegram-бот на сервере не настроен"}
		}
		var in struct {
			What string `json:"what"`
		}
		if err := decode(r, &in); err != nil {
			return err
		}
		var err error
		if in.What == "backup" {
			err = a.tg.SendBackup(r.Context(), uid)
		} else {
			err = a.tg.SendSummary(r.Context(), uid, false)
		}
		var ae *APIError
		if err != nil && !errors.As(err, &ae) {
			a.log.Warn("telegram: send", "user", uid, "err", err)
			return &APIError{http.StatusBadGateway, "Telegram не принял сообщение — попробуйте позже"}
		}
		return err
	}))

	mux.HandleFunc("POST /api/rates/refresh", a.withState(func(r *http.Request, _ int64) error {
		if err := a.rates.Refresh(r.Context()); err != nil {
			return &APIError{http.StatusBadGateway, "Не удалось обновить курсы: " + err.Error()}
		}
		return nil
	}))
	mux.HandleFunc("POST /api/data/reset", a.withData(func(r *http.Request, uid int64) error {
		return s.Reset(r.Context(), uid)
	}))
	mux.HandleFunc("POST /api/import", a.importCSV)

	mux.HandleFunc("POST /api/backups", a.withState(func(r *http.Request, uid int64) error {
		return s.CreateBackup(r.Context(), uid)
	}))
	mux.HandleFunc("PUT /api/backups/{id}", a.withState(withIDBody(s.RenameBackup)))
	mux.HandleFunc("DELETE /api/backups/{id}", a.withState(withID(s.DeleteBackup)))
	mux.HandleFunc("GET /api/backups/{id}/file", a.backupFile)
	mux.HandleFunc("POST /api/backups/{id}/restore", a.withState(withID(s.RestoreBackup)))
	// тело — сам файл бекапа, его JSON как есть
	mux.HandleFunc("POST /api/backups/restore", a.withState(func(r *http.Request, uid int64) error {
		raw, err := io.ReadAll(r.Body)
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			return badRequest(fmt.Sprintf("Файл больше %d МБ", maxBackupBytes>>20))
		}
		if err != nil {
			return badRequest("Файл не дошёл: " + err.Error())
		}
		return s.RestoreFile(r.Context(), uid, raw)
	}))
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

// withData — withState для изменений данных: после них данные уже не совпадают
// с текущей версией из бекапов.
func (a *API) withData(fn func(r *http.Request, uid int64) error) http.HandlerFunc {
	return a.withState(func(r *http.Request, uid int64) error {
		if err := fn(r, uid); err != nil {
			return err
		}
		a.checkBudgets(uid)
		return a.store.DataChanged(r.Context(), uid)
	})
}

// checkBudgets в фоне проверяет, не пора ли уведомить о бюджетах.
func (a *API) checkBudgets(uid int64) {
	if a.push != nil {
		go a.push.CheckBudgets(uid)
	}
}

// pushTest шлёт пробное уведомление на все устройства пользователя.
func (a *API) pushTest(w http.ResponseWriter, r *http.Request) {
	u, ok := a.requireUser(w, r)
	if !ok {
		return
	}
	if a.push == nil {
		a.fail(w, r, &APIError{http.StatusServiceUnavailable, "Пуш-уведомления на сервере не настроены"})
		return
	}
	n, err := a.push.Send(r.Context(), u.ID, pushMessage{
		Title: "Penny", Body: "Уведомления работают — сюда придут предупреждения о бюджетах.", URL: "/?screen=budgets", Tag: "test",
	})
	if err != nil {
		a.fail(w, r, err)
		return
	}
	if n == 0 {
		a.fail(w, r, badRequest("Ни одно устройство не приняло уведомление — включите уведомления заново"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"sent": n})
}

// searchCoins ищет монеты в CoinGecko и помечает, какие можно добавить.
func (a *API) searchCoins(w http.ResponseWriter, r *http.Request) {
	u, ok := a.requireUser(w, r)
	if !ok {
		return
	}
	found, err := a.rates.SearchCoins(r.Context(), r.URL.Query().Get("q"))
	if err != nil {
		a.log.Warn("coin search", "err", err)
		a.fail(w, r, &APIError{http.StatusBadGateway, "Поиск CoinGecko сейчас не отвечает — попробуйте через минуту"})
		return
	}
	if err := a.store.MarkFound(r.Context(), u.ID, found); err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"coins": found})
}

// telegramLink выдаёт ссылку t.me/<бот>?start=<код> для привязки чата.
func (a *API) telegramLink(w http.ResponseWriter, r *http.Request) {
	u, ok := a.requireUser(w, r)
	if !ok {
		return
	}
	if a.tg == nil {
		a.fail(w, r, &APIError{http.StatusServiceUnavailable, "Telegram-бот на сервере не настроен"})
		return
	}
	bot := a.tg.Username()
	if bot == "" {
		_, why := a.tg.status()
		a.fail(w, r, &APIError{http.StatusServiceUnavailable, "Бот не на связи: " + why})
		return
	}
	code, err := a.store.TelegramLink(r.Context(), u.ID)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"url": "https://t.me/" + bot + "?start=" + code})
}

func (a *API) state(ctx context.Context, u *User) (*State, error) {
	st, err := a.store.State(ctx, u.ID)
	if err != nil {
		return nil, err
	}
	st.User = u
	if a.push != nil {
		st.PushKey = a.push.PublicKey()
	}
	if st.Telegram, err = a.store.telegramState(ctx, u.ID); err != nil {
		return nil, err
	}
	if st.Telegram.Enabled = a.tg != nil; st.Telegram.Enabled {
		st.Telegram.Ready, st.Telegram.Error = a.tg.status()
	}
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
	if err == nil {
		a.checkBudgets(u.ID)
		err = a.store.DataChanged(r.Context(), u.ID)
	}
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

// backupFile отдаёт бекап файлом. Имя — по времени создания, по Москве.
func (a *API) backupFile(w http.ResponseWriter, r *http.Request) {
	u, ok := a.requireUser(w, r)
	if !ok {
		return
	}
	id, err := pathID(r)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	data, at, err := a.store.BackupJSON(r.Context(), u.ID, id)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	name := "penny-backup-" + at.In(moscow).Format("2006-01-02-1504") + ".json"
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+name+`"`)
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(data)
}

// moscow — пояс для имён файлов; UTC+3 без переходов, tzdata в образе не нужна.
var moscow = time.FixedZone("MSK", 3*60*60)

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
		limit := int64(5 << 20)
		if r.URL.Path == "/api/backups/restore" {
			limit = maxBackupBytes
		}
		r.Body = http.MaxBytesReader(w, r.Body, limit)
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
