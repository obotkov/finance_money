package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Telegram-бот Penny: шлёт сводку по финансам и файл бекапа — по понедельникам
// в 10:00 по Москве и по кнопке. Сервер сам забирает сообщения бота длинным
// опросом getUpdates, вебхук не нужен. Чат привязывается к пользователю
// командой /start <код>: код даёт сайт в ссылке t.me/<бот>?start=<код>.
// Без TELEGRAM_BOT_TOKEN бот выключен.

const (
	telegramLinkTTL = 15 * time.Minute
	weeklyHour      = 10 // по Москве, в понедельник
)

type Telegram struct {
	store    *Store
	log      *slog.Logger
	api      string // https://api.telegram.org/bot<token>
	http     *http.Client
	username string // имя бота для ссылки; пусто, пока getMe не ответил
}

func NewTelegram(store *Store, token, apiURL string, log *slog.Logger) *Telegram {
	if token == "" {
		return nil
	}
	if apiURL == "" {
		apiURL = "https://api.telegram.org"
	}
	return &Telegram{store: store, log: log, api: strings.TrimRight(apiURL, "/") + "/bot" + token,
		http: &http.Client{Timeout: 70 * time.Second}}
}

// call вызывает метод Bot API с JSON-параметрами; out — поле result ответа.
func (t *Telegram) call(ctx context.Context, method string, params, out any) error {
	body, err := json.Marshal(params)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.api+"/"+method, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	return t.do(req, out)
}

func (t *Telegram) do(req *http.Request, out any) error {
	resp, err := t.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	var r struct {
		OK          bool            `json:"ok"`
		Description string          `json:"description"`
		Result      json.RawMessage `json:"result"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&r); err != nil {
		return fmt.Errorf("telegram %s: %s", req.URL.Path[strings.LastIndex(req.URL.Path, "/")+1:], resp.Status)
	}
	if !r.OK {
		return fmt.Errorf("telegram: %s", r.Description)
	}
	if out != nil {
		return json.Unmarshal(r.Result, out)
	}
	return nil
}

func (t *Telegram) sendMessage(ctx context.Context, chat int64, text string) error {
	return t.call(ctx, "sendMessage", map[string]any{
		"chat_id": chat, "text": text, "parse_mode": "HTML", "link_preview_options": map[string]bool{"is_disabled": true},
	}, nil)
}

func (t *Telegram) sendDocument(ctx context.Context, chat int64, name string, data []byte, caption string) error {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("chat_id", fmt.Sprint(chat))
	_ = mw.WriteField("caption", caption)
	fw, err := mw.CreateFormFile("document", name)
	if err != nil {
		return err
	}
	if _, err := fw.Write(data); err != nil {
		return err
	}
	if err := mw.Close(); err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, t.api+"/sendDocument", &buf)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	return t.do(req, nil)
}

// Run узнаёт имя бота, затем забирает сообщения и раз в 10 минут проверяет,
// не пора ли разослать недельную сводку.
func (t *Telegram) Run(ctx context.Context) {
	for t.username == "" {
		var me struct {
			Username string `json:"username"`
		}
		if err := t.call(ctx, "getMe", map[string]any{}, &me); err != nil {
			t.log.Warn("telegram: getMe", "err", err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(time.Minute):
			}
			continue
		}
		t.username = me.Username
		t.log.Info("telegram: bot ready", "bot", me.Username)
	}
	// getUpdates не работает, пока у бота стоит вебхук
	_ = t.call(ctx, "deleteWebhook", map[string]any{}, nil)
	go t.poll(ctx)
	t.weekly(ctx)
	tick := time.NewTicker(10 * time.Minute)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			t.weekly(ctx)
		}
	}
}

type tgUpdate struct {
	ID      int64 `json:"update_id"`
	Message *struct {
		Text string `json:"text"`
		Chat struct {
			ID   int64  `json:"id"`
			Type string `json:"type"`
		} `json:"chat"`
		From struct {
			Username  string `json:"username"`
			FirstName string `json:"first_name"`
		} `json:"from"`
	} `json:"message"`
}

func (t *Telegram) poll(ctx context.Context) {
	var offset int64
	for ctx.Err() == nil {
		var updates []tgUpdate
		err := t.call(ctx, "getUpdates", map[string]any{"offset": offset, "timeout": 50, "allowed_updates": []string{"message"}}, &updates)
		if err != nil {
			if ctx.Err() == nil {
				t.log.Warn("telegram: getUpdates", "err", err)
				time.Sleep(10 * time.Second)
			}
			continue
		}
		for _, u := range updates {
			offset = u.ID + 1
			if u.Message != nil && u.Message.Chat.Type == "private" {
				t.handle(ctx, u)
			}
		}
	}
}

const tgHelp = "Команды:\n/summary — сводка сейчас\n/backup — бекап сейчас\n/stop — отключить Penny от этого чата"

func (t *Telegram) handle(ctx context.Context, u tgUpdate) {
	m := u.Message
	chat := m.Chat.ID
	cmd, arg, _ := strings.Cut(strings.TrimSpace(m.Text), " ")
	cmd = strings.ToLower(strings.SplitN(cmd, "@", 2)[0]) // /start@PennyBot → /start
	reply := func(text string) {
		if err := t.sendMessage(ctx, chat, text); err != nil {
			t.log.Warn("telegram: reply", "err", err)
		}
	}
	if cmd == "/start" && arg != "" {
		name := m.From.Username
		if name != "" {
			name = "@" + name
		} else {
			name = m.From.FirstName
		}
		email, err := t.store.LinkTelegram(ctx, strings.TrimSpace(arg), chat, name)
		if err != nil {
			reply("Ссылка устарела или уже использована. Нажмите «Подключить Telegram» в Настройках Penny ещё раз.")
			return
		}
		reply("Готово — Penny подключён к аккаунту " + escapeTg(email) + ".\nСводка и бекап будут приходить по понедельникам в 10:00 по Москве.\n\n" + tgHelp)
		return
	}
	uid, err := t.store.TelegramUser(ctx, chat)
	if err != nil {
		reply("Этот чат не подключён. Откройте penny.place → Настройки → Telegram и нажмите «Подключить Telegram».")
		return
	}
	switch cmd {
	case "/summary":
		if err := t.SendSummary(ctx, uid, false); err != nil {
			reply("Не получилось собрать сводку, попробуйте позже.")
		}
	case "/backup":
		if err := t.SendBackup(ctx, uid); err != nil {
			reply("Не получилось сделать бекап, попробуйте позже.")
		}
	case "/stop":
		if err := t.store.UnlinkTelegram(ctx, uid); err == nil {
			reply("Отключено. Подключить снова можно в Настройках Penny.")
		}
	default:
		reply(tgHelp)
	}
}

func escapeTg(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

// SendSummary отправляет сводку в чат пользователя; weekly — за прошлую неделю.
func (t *Telegram) SendSummary(ctx context.Context, uid int64, weekly bool) error {
	chat, err := t.store.telegramChat(ctx, uid)
	if err != nil {
		return err
	}
	text, err := t.store.FinanceSummary(ctx, uid, weekly)
	if err != nil {
		return err
	}
	return t.sendMessage(ctx, chat, text)
}

// SendBackup сохраняет бекап (он же появится в списке на сайте) и отправляет его файлом.
func (t *Telegram) SendBackup(ctx context.Context, uid int64) error {
	chat, err := t.store.telegramChat(ctx, uid)
	if err != nil {
		return err
	}
	id, err := t.store.createBackup(ctx, uid, "telegram")
	if err != nil {
		return err
	}
	data, at, err := t.store.BackupJSON(ctx, uid, id)
	if err != nil {
		return err
	}
	name := "penny-backup-" + at.In(moscow).Format("2006-01-02-1504") + ".json"
	return t.sendDocument(ctx, chat, name, data,
		"Бекап Penny на "+at.In(moscow).Format("02.01.2006 15:04")+". Восстановить: Настройки → Бекапы → «Восстановить из файла».")
}

// weekly рассылает сводку и бекап тем, кому их на этой неделе ещё не слали,
// если уже наступил понедельник 10:00 по Москве. Сервер, лежавший в это время,
// догонит рассылку, как только поднимется.
func (t *Telegram) weekly(ctx context.Context) {
	now := time.Now().In(moscow)
	monday := time.Date(now.Year(), now.Month(), now.Day(), weeklyHour, 0, 0, 0, moscow).AddDate(0, 0, -((int(now.Weekday()) + 6) % 7))
	if now.Before(monday) {
		return
	}
	week := isoWeek(now)
	rows, _ := t.store.db.Query(ctx, `
		SELECT id FROM users WHERE telegram_chat_id IS NOT NULL AND telegram_weekly AND telegram_sent_week <> $1`, week)
	ids, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		t.log.Warn("telegram: weekly users", "err", err)
		return
	}
	for _, uid := range ids {
		// отмечаем до отправки: упавшая отправка не должна слать по кругу каждые 10 минут
		if _, err := t.store.db.Exec(ctx, `UPDATE users SET telegram_sent_week = $2 WHERE id = $1`, uid, week); err != nil {
			continue
		}
		if err := t.SendSummary(ctx, uid, true); err != nil {
			t.log.Warn("telegram: weekly summary", "user", uid, "err", err)
		}
		if err := t.SendBackup(ctx, uid); err != nil {
			t.log.Warn("telegram: weekly backup", "user", uid, "err", err)
		}
		t.log.Info("telegram: weekly sent", "user", uid, "week", week)
	}
}

func isoWeek(t time.Time) string {
	y, w := t.ISOWeek()
	return fmt.Sprintf("%d-W%02d", y, w)
}

// ---- хранение ----

// TelegramLink выдаёт одноразовый код привязки чата.
func (s *Store) TelegramLink(ctx context.Context, uid int64) (string, error) {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	code := hex.EncodeToString(b)
	_, err := s.db.Exec(ctx, `
		WITH gone AS (DELETE FROM telegram_links WHERE expires_at < now() OR user_id = $1)
		INSERT INTO telegram_links (code, user_id, expires_at) VALUES ($2, $1, now() + $3::interval)`,
		uid, code, fmt.Sprintf("%d seconds", int(telegramLinkTTL.Seconds())))
	return code, err
}

// LinkTelegram привязывает чат по коду и возвращает почту пользователя. Неделя
// отмечается отправленной: подключившись в среду, сводку ждут в понедельник,
// а не сразу.
func (s *Store) LinkTelegram(ctx context.Context, code string, chat int64, name string) (string, error) {
	var email string
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		var uid int64
		err := tx.QueryRow(ctx, `DELETE FROM telegram_links WHERE code = $1 AND expires_at > now() RETURNING user_id`, code).Scan(&uid)
		if err != nil {
			return err
		}
		// один чат — один пользователь: прежняя привязка этого чата снимается
		if _, err := tx.Exec(ctx, `UPDATE users SET telegram_chat_id = NULL WHERE telegram_chat_id = $1 AND id <> $2`, chat, uid); err != nil {
			return err
		}
		return tx.QueryRow(ctx, `
			UPDATE users SET telegram_chat_id = $2, telegram_name = $3, telegram_sent_week = $4
			WHERE id = $1 RETURNING email`, uid, chat, name, isoWeek(time.Now().In(moscow))).Scan(&email)
	})
	return email, err
}

func (s *Store) UnlinkTelegram(ctx context.Context, uid int64) error {
	_, err := s.db.Exec(ctx, `UPDATE users SET telegram_chat_id = NULL, telegram_name = '' WHERE id = $1`, uid)
	return err
}

func (s *Store) TelegramUser(ctx context.Context, chat int64) (int64, error) {
	var uid int64
	err := s.db.QueryRow(ctx, `SELECT id FROM users WHERE telegram_chat_id = $1`, chat).Scan(&uid)
	return uid, err
}

func (s *Store) telegramChat(ctx context.Context, uid int64) (int64, error) {
	var chat *int64
	if err := s.db.QueryRow(ctx, `SELECT telegram_chat_id FROM users WHERE id = $1`, uid).Scan(&chat); err != nil {
		return 0, err
	}
	if chat == nil {
		return 0, badRequest("Telegram не подключён")
	}
	return *chat, nil
}

// TelegramState — что показать в Настройках.
type TelegramState struct {
	Enabled bool   `json:"enabled"` // бот настроен на сервере
	Linked  bool   `json:"linked"`
	Name    string `json:"name,omitempty"`
	Weekly  bool   `json:"weekly"`
}

func (s *Store) telegramState(ctx context.Context, uid int64) (TelegramState, error) {
	var st TelegramState
	var chat *int64
	err := s.db.QueryRow(ctx, `SELECT telegram_chat_id, telegram_name, telegram_weekly FROM users WHERE id = $1`, uid).
		Scan(&chat, &st.Name, &st.Weekly)
	if errors.Is(err, pgx.ErrNoRows) {
		return st, nil
	}
	st.Linked = chat != nil
	return st, err
}

func (s *Store) SetTelegramWeekly(ctx context.Context, uid int64, on bool) error {
	_, err := s.db.Exec(ctx, `UPDATE users SET telegram_weekly = $2 WHERE id = $1`, uid, on)
	return err
}
