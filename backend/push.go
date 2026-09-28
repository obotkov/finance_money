package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"strings"
	"time"

	webpush "github.com/SherClockHolmes/webpush-go"
	"github.com/jackc/pgx/v5"
)

// Пуш-уведомления через Web Push: браузер подписывается ключом VAPID сервера,
// сервер шифрует сообщение под подписку и отдаёт его push-сервису браузера.
// Ключи VAPID создаются при первом запуске и лежат в server_settings — смена
// ключей отвязала бы все подписки.

type PushSubscription struct {
	Endpoint string `json:"endpoint"`
	Keys     struct {
		P256dh string `json:"p256dh"`
		Auth   string `json:"auth"`
	} `json:"keys"`
}

// pushMessage — то, что показывает service worker (web/sw.js).
type pushMessage struct {
	Title string `json:"title"`
	Body  string `json:"body"`
	URL   string `json:"url"`
	Tag   string `json:"tag,omitempty"`
}

type Pusher struct {
	store      *Store
	log        *slog.Logger
	subscriber string // https-адрес сайта или почта — push-сервисы требуют контакт
	pub, priv  string
}

func NewPusher(ctx context.Context, store *Store, subscriber string, log *slog.Logger) (*Pusher, error) {
	p := &Pusher{store: store, log: log, subscriber: subscriber}
	load := func() error {
		var pub, priv *string
		err := store.db.QueryRow(ctx, `
			SELECT max(value) FILTER (WHERE key = 'vapid_public'), max(value) FILTER (WHERE key = 'vapid_private')
			FROM server_settings`).Scan(&pub, &priv)
		if err == nil && pub != nil && priv != nil {
			p.pub, p.priv = *pub, *priv
		}
		return err
	}
	if err := load(); err != nil {
		return nil, err
	}
	if p.pub != "" {
		return p, nil
	}
	priv, pub, err := webpush.GenerateVAPIDKeys()
	if err != nil {
		return nil, err
	}
	// два запуска разом не заведут разные ключи: остаётся вставка первого
	if _, err := store.db.Exec(ctx, `
		INSERT INTO server_settings (key, value) VALUES ('vapid_public', $1), ('vapid_private', $2)
		ON CONFLICT (key) DO NOTHING`, pub, priv); err != nil {
		return nil, err
	}
	log.Info("push: VAPID keys created")
	return p, load()
}

// PublicKey — ключ, которым браузер подписывается на пуши.
func (p *Pusher) PublicKey() string { return p.pub }

func (s *Store) SavePushSubscription(ctx context.Context, uid int64, sub PushSubscription) error {
	if !strings.HasPrefix(sub.Endpoint, "https://") || sub.Keys.P256dh == "" || sub.Keys.Auth == "" {
		return badRequest("Браузер прислал неполную подписку на уведомления")
	}
	_, err := s.db.Exec(ctx, `
		INSERT INTO push_subscriptions (user_id, endpoint, p256dh, auth) VALUES ($1, $2, $3, $4)
		ON CONFLICT (endpoint) DO UPDATE SET user_id = $1, p256dh = $3, auth = $4`,
		uid, sub.Endpoint, sub.Keys.P256dh, sub.Keys.Auth)
	return err
}

func (s *Store) DeletePushSubscription(ctx context.Context, uid int64, endpoint string) error {
	_, err := s.db.Exec(ctx, `DELETE FROM push_subscriptions WHERE user_id = $1 AND endpoint = $2`, uid, endpoint)
	return err
}

func (s *Store) pushDevices(ctx context.Context, uid int64) (int, error) {
	var n int
	err := s.db.QueryRow(ctx, `SELECT count(*) FROM push_subscriptions WHERE user_id = $1`, uid).Scan(&n)
	return n, err
}

// Send отправляет сообщение на все устройства пользователя и возвращает,
// сколько push-сервисов его приняли. Отозванные подписки (404, 410) удаляются.
func (p *Pusher) Send(ctx context.Context, uid int64, msg pushMessage) (int, error) {
	rows, _ := p.store.db.Query(ctx, `SELECT endpoint, p256dh, auth FROM push_subscriptions WHERE user_id = $1`, uid)
	subs, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (webpush.Subscription, error) {
		var s webpush.Subscription
		err := r.Scan(&s.Endpoint, &s.Keys.P256dh, &s.Keys.Auth)
		return s, err
	})
	if err != nil {
		return 0, err
	}
	payload, err := json.Marshal(msg)
	if err != nil {
		return 0, err
	}
	sent := 0
	for _, sub := range subs {
		resp, err := webpush.SendNotificationWithContext(ctx, payload, &sub, &webpush.Options{
			Subscriber:      p.subscriber,
			VAPIDPublicKey:  p.pub,
			VAPIDPrivateKey: p.priv,
			TTL:             24 * 60 * 60,
			Urgency:         webpush.UrgencyNormal,
			Topic:           msg.Tag,
		})
		if err != nil {
			p.log.Warn("push: send", "user", uid, "err", err)
			continue
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		resp.Body.Close()
		switch {
		case resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone:
			_ = p.store.DeletePushSubscription(ctx, uid, sub.Endpoint)
		case resp.StatusCode >= 300:
			p.log.Warn("push: refused", "user", uid, "status", resp.StatusCode, "body", string(body))
		default:
			sent++
		}
	}
	return sent, nil
}

// CheckBudgets шлёт уведомления по бюджетам, которые дошли до порога или
// превышены. Вызывается после каждого изменения данных, в фоне.
func (p *Pusher) CheckBudgets(uid int64) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	due, levels, err := p.store.dueBudgetAlerts(ctx, uid)
	if err != nil {
		p.log.Warn("budgets: check", "user", uid, "err", err)
		return
	}
	for i, u := range due {
		if _, err := p.Send(ctx, uid, budgetMessage(u, levels[i])); err != nil {
			p.log.Warn("budgets: push", "user", uid, "err", err)
		}
	}
}

func budgetMessage(u budgetUsage, level string) pushMessage {
	pct := int(math.Floor(u.Spent / u.Limit * 100))
	m := pushMessage{URL: "/?screen=budgets", Tag: fmt.Sprintf("budget-%d", u.ID)}
	if level == "over" {
		m.Title = u.Category + ": лимит превышен"
		m.Body = "Потрачено " + rub(u.Spent) + " из " + rub(u.Limit) + " — на " + rub(u.Spent-u.Limit) + " больше."
		return m
	}
	m.Title = fmt.Sprintf("%s: %d %% лимита", u.Category, pct)
	m.Body = "Потрачено " + rub(u.Spent) + " из " + rub(u.Limit) + ", осталось " + rub(u.Limit-u.Spent) + " до конца месяца."
	return m
}

// rub — «12 345 ₽»: целые рубли с пробелами между разрядами.
func rub(v float64) string {
	n := int64(math.Round(v))
	neg := n < 0
	if neg {
		n = -n
	}
	s := fmt.Sprint(n)
	var b strings.Builder
	for i, c := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteRune(' ')
		}
		b.WriteRune(c)
	}
	if neg {
		return "−" + b.String() + " ₽"
	}
	return b.String() + " ₽"
}
