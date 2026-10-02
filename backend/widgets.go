package main

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
)

// Widget — карточка крипто-портфеля или крипто-счёта на Сводке.
// Source: "all" — вся криптовалюта, "p:<id портфеля>", "a:<id счёта>".
type Widget struct {
	Source string `json:"source"`
	Size   string `json:"size"` // small — в половину ширины, wide — во всю, с графиками монет
	Rows   int    `json:"rows"` // сколько монет показать, самые дорогие сверху
	Cur    string `json:"cur"`  // USDT или RUB
}

const maxWidgets = 12

func (w Widget) validate() error {
	switch {
	case w.Source == "all":
	case strings.HasPrefix(w.Source, "p:") || strings.HasPrefix(w.Source, "a:"):
		if id, err := strconv.ParseInt(w.Source[2:], 10, 64); err != nil || id <= 0 {
			return badRequest("Неизвестный источник виджета")
		}
	default:
		return badRequest("Неизвестный источник виджета")
	}
	if w.Size != "small" && w.Size != "wide" {
		return badRequest("Размер виджета — small или wide")
	}
	if w.Rows < 0 || w.Rows > 10 {
		return badRequest("В виджете от 0 до 10 строк")
	}
	if w.Cur != "USDT" && w.Cur != "RUB" {
		return badRequest("Валюта виджета — USDT или RUB")
	}
	return nil
}

// widgets возвращает настройки виджетов; nil — их ещё не настраивали.
func (s *Store) widgets(ctx context.Context, uid int64) ([]Widget, error) {
	var raw []byte
	if err := s.db.QueryRow(ctx, `SELECT widgets FROM users WHERE id = $1`, uid).Scan(&raw); err != nil || raw == nil {
		return nil, err
	}
	list := []Widget{}
	return list, json.Unmarshal(raw, &list)
}

// SetWidgets сохраняет виджеты целиком, в том порядке, в каком они на Сводке.
func (s *Store) SetWidgets(ctx context.Context, uid int64, in struct {
	Widgets []Widget `json:"widgets"`
}) error {
	if in.Widgets == nil {
		in.Widgets = []Widget{}
	}
	if len(in.Widgets) > maxWidgets {
		return badRequest("Виджетов может быть не больше " + strconv.Itoa(maxWidgets))
	}
	for _, w := range in.Widgets {
		if err := w.validate(); err != nil {
			return err
		}
	}
	raw, err := json.Marshal(in.Widgets)
	if err != nil {
		return err
	}
	_, err = s.db.Exec(ctx, `UPDATE users SET widgets = $2 WHERE id = $1`, uid, raw)
	return err
}

// widgetSQL — запрос имени счёта или портфеля по id и обратно, по префиксу источника.
var widgetSQL = map[string][2]string{
	"a:": {`SELECT name FROM accounts WHERE id = $1 AND user_id = $2`, `SELECT id FROM accounts WHERE name = $1 AND user_id = $2`},
	"p:": {`SELECT name FROM crypto_portfolios WHERE id = $1 AND user_id = $2`, `SELECT id FROM crypto_portfolios WHERE name = $1 AND user_id = $2`},
}

// widgetNames заменяет в источниках виджетов id на имя (перед восстановлением бекапа).
// nil — виджеты не настраивались.
func widgetNames(ctx context.Context, tx pgx.Tx, uid int64) ([]Widget, error) {
	var raw []byte
	if err := tx.QueryRow(ctx, `SELECT widgets FROM users WHERE id = $1`, uid).Scan(&raw); err != nil || raw == nil {
		return nil, err
	}
	var list []Widget
	if err := json.Unmarshal(raw, &list); err != nil {
		return nil, nil // испорченные настройки просто сбрасываются
	}
	out := []Widget{}
	for _, w := range list {
		if q, ok := widgetSQL[w.Source[:min(2, len(w.Source))]]; ok {
			id, _ := strconv.ParseInt(w.Source[2:], 10, 64)
			var name string
			if err := tx.QueryRow(ctx, q[0], id, uid).Scan(&name); errors.Is(err, pgx.ErrNoRows) {
				continue // счёта или портфеля уже нет
			} else if err != nil {
				return nil, err
			}
			w.Source = w.Source[:2] + name
		}
		out = append(out, w)
	}
	return out, nil
}

// widgetIDs — обратно: имя в источнике на id восстановленного счёта или портфеля.
// Виджеты, чьих счетов и портфелей в бекапе нет, пропадают.
func widgetIDs(ctx context.Context, tx pgx.Tx, uid int64, list []Widget) error {
	if list == nil {
		return nil
	}
	out := []Widget{}
	for _, w := range list {
		if q, ok := widgetSQL[w.Source[:min(2, len(w.Source))]]; ok {
			var id int64
			if err := tx.QueryRow(ctx, q[1], w.Source[2:], uid).Scan(&id); errors.Is(err, pgx.ErrNoRows) {
				continue
			} else if err != nil {
				return err
			}
			w.Source = w.Source[:2] + strconv.FormatInt(id, 10)
		}
		out = append(out, w)
	}
	raw, err := json.Marshal(out)
	if err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE users SET widgets = $2 WHERE id = $1`, uid, raw)
	return err
}
