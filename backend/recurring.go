package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Повторяющаяся операция — правило: расход или доход на счёт с частотой. В день
// срока (по Москве) сервер записывает обычную операцию через insertTx, так что
// остаток счёта меняется как при ручной записи. Если дата начала в прошлом,
// операции за прошедшие сроки создаются сразу.

// частоты: код → подпись для ошибок
var recurringFreqs = map[string]string{
	"day": "каждый день", "week": "каждую неделю", "2weeks": "каждые две недели",
	"month": "каждый месяц", "quarter": "каждый квартал", "year": "каждый год",
}

// maxCatchUp — сколько операций за прошедшие сроки можно создать разом
const maxCatchUp = 400

type Recurring struct {
	ID       int64   `json:"id"`
	Type     string  `json:"type"`
	Account  string  `json:"account"`
	Amount   float64 `json:"amount"`
	Category string  `json:"category"`
	Title    string  `json:"title"`
	Freq     string  `json:"freq"`
	Start    string  `json:"start"`
	Last     string  `json:"last,omitempty"`
	Next     string  `json:"next"`
}

type RecurringInput struct {
	Type     string  `json:"type"`
	Account  string  `json:"account"`
	Amount   float64 `json:"amount"`
	Category string  `json:"category"`
	Title    string  `json:"title"`
	Freq     string  `json:"freq"`
	Start    string  `json:"start"`
}

func (in *RecurringInput) validate() (time.Time, error) {
	in.Title, in.Category = strings.TrimSpace(in.Title), strings.TrimSpace(in.Category)
	if in.Type != "expense" && in.Type != "income" {
		return time.Time{}, badRequest("Повторять можно расход или доход")
	}
	if in.Account == "" {
		return time.Time{}, badRequest("Выберите счёт")
	}
	if !(in.Amount > 0) {
		return time.Time{}, badRequest("Сумма должна быть больше нуля")
	}
	if in.Category == "" {
		return time.Time{}, badRequest("Выберите категорию")
	}
	if recurringFreqs[in.Freq] == "" {
		return time.Time{}, badRequest("Выберите частоту")
	}
	start, err := time.Parse(time.DateOnly, in.Start)
	if err != nil {
		return time.Time{}, badRequest("Дата начала должна быть в формате ГГГГ-ММ-ДД")
	}
	return start, nil
}

// occurrence — n-й срок правила, считая от даты начала (n = 0 — сама дата).
// Месяцы прибавляются к дате начала, а не к прошлому сроку: 31 января →
// 28 февраля → 31 марта.
func occurrence(start time.Time, freq string, n int) time.Time {
	switch freq {
	case "day":
		return start.AddDate(0, 0, n)
	case "week":
		return start.AddDate(0, 0, 7*n)
	case "2weeks":
		return start.AddDate(0, 0, 14*n)
	case "month":
		return addMonths(start, n)
	case "quarter":
		return addMonths(start, 3*n)
	default: // year
		return addMonths(start, 12*n)
	}
}

// addMonths прибавляет месяцы, не перескакивая в следующий: день, которого
// в месяце нет, становится последним днём месяца.
func addMonths(t time.Time, months int) time.Time {
	first := time.Date(t.Year(), t.Month()+time.Month(months), 1, 0, 0, 0, 0, time.UTC)
	last := first.AddDate(0, 1, -1).Day()
	day := t.Day()
	if day > last {
		day = last
	}
	return time.Date(first.Year(), first.Month(), day, 0, 0, 0, 0, time.UTC)
}

// nextAfter — первый срок правила позже after (или сама дата начала, если
// она позже after).
func nextAfter(start time.Time, freq string, after time.Time) time.Time {
	if after.Before(start) {
		return start
	}
	// прикидка по дням, чтобы не перебирать годы ежедневных сроков с нуля
	n := 0
	if days := int(after.Sub(start).Hours() / 24); freq == "day" {
		n = days
	} else if freq == "week" {
		n = days / 7
	} else if freq == "2weeks" {
		n = days / 14
	}
	for !occurrence(start, freq, n).After(after) {
		n++
	}
	return occurrence(start, freq, n)
}

// dueCount — сколько сроков от from до today включительно.
func dueCount(start time.Time, freq string, from, today time.Time) int {
	n := 0
	for d := from; !d.After(today) && n <= maxCatchUp; d = nextAfter(start, freq, d) {
		n++
	}
	return n
}

// moscowToday — сегодняшняя дата по Москве, в полночь UTC, как даты из базы.
func moscowToday() time.Time {
	y, m, d := time.Now().In(moscow).Date()
	return time.Date(y, m, d, 0, 0, 0, 0, time.UTC)
}

func (s *Store) recurring(ctx context.Context, uid int64) ([]Recurring, error) {
	rows, _ := s.db.Query(ctx, `
		SELECT r.id, r.type, a.name, r.amount, r.category, r.title, r.freq, to_char(r.start_date, 'YYYY-MM-DD'),
		       COALESCE(to_char(r.last_date, 'YYYY-MM-DD'), ''), to_char(r.next_date, 'YYYY-MM-DD')
		FROM recurring r JOIN accounts a ON a.id = r.account_id
		WHERE r.user_id = $1 ORDER BY r.id`, uid)
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Recurring, error) {
		var r Recurring
		err := row.Scan(&r.ID, &r.Type, &r.Account, &r.Amount, &r.Category, &r.Title, &r.Freq, &r.Start, &r.Last, &r.Next)
		return r, err
	})
}

// CreateRecurring заводит правило и сразу создаёт операции за сроки до сегодня.
func (s *Store) CreateRecurring(ctx context.Context, uid int64, in RecurringInput) error {
	start, err := in.validate()
	if err != nil {
		return err
	}
	if n := dueCount(start, in.Freq, start, moscowToday()); n > maxCatchUp {
		return badRequest(fmt.Sprintf("С такой даты начала пришлось бы создать больше %d операций — выберите дату позже", maxCatchUp))
	}
	return pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		acc, err := lockAccount(ctx, tx, uid, in.Account)
		if err != nil {
			return err
		}
		var id int64
		if err := tx.QueryRow(ctx, `
			INSERT INTO recurring (user_id, type, account_id, amount, category, title, freq, start_date, next_date)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $8) RETURNING id`,
			uid, in.Type, acc.id, in.Amount, in.Category, in.Title, in.Freq, start).Scan(&id); err != nil {
			return err
		}
		_, err = applyRecurring(ctx, tx, uid, id, moscowToday())
		return err
	})
}

// UpdateRecurring меняет правило. Уже созданные операции остаются как есть,
// следующий срок считается после последней из них, чтобы ничего не задвоилось.
func (s *Store) UpdateRecurring(ctx context.Context, uid, id int64, in RecurringInput) error {
	start, err := in.validate()
	if err != nil {
		return err
	}
	return pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		var last *time.Time
		err := tx.QueryRow(ctx, `SELECT last_date FROM recurring WHERE id = $1 AND user_id = $2 FOR UPDATE`, id, uid).Scan(&last)
		if errors.Is(err, pgx.ErrNoRows) {
			return notFound("Повторяющаяся операция не найдена")
		}
		if err != nil {
			return err
		}
		next := start
		if last != nil {
			next = nextAfter(start, in.Freq, *last)
		}
		if n := dueCount(start, in.Freq, next, moscowToday()); n > maxCatchUp {
			return badRequest(fmt.Sprintf("С такой даты начала пришлось бы создать больше %d операций — выберите дату позже", maxCatchUp))
		}
		acc, err := lockAccount(ctx, tx, uid, in.Account)
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			UPDATE recurring SET type = $3, account_id = $4, amount = $5, category = $6, title = $7, freq = $8,
			                     start_date = $9, next_date = $10
			WHERE id = $1 AND user_id = $2`,
			id, uid, in.Type, acc.id, in.Amount, in.Category, in.Title, in.Freq, start, next); err != nil {
			return err
		}
		_, err = applyRecurring(ctx, tx, uid, id, moscowToday())
		return err
	})
}

// DeleteRecurring удаляет правило; созданные им операции остаются.
func (s *Store) DeleteRecurring(ctx context.Context, uid, id int64) error {
	tag, err := s.db.Exec(ctx, `DELETE FROM recurring WHERE id = $1 AND user_id = $2`, id, uid)
	if err == nil && tag.RowsAffected() == 0 {
		return notFound("Повторяющаяся операция не найдена")
	}
	return err
}

// applyRecurring записывает операции правила за все сроки до today и
// передвигает следующий срок. Возвращает, сколько операций создано.
func applyRecurring(ctx context.Context, tx pgx.Tx, uid, id int64, today time.Time) (int, error) {
	var r RecurringInput
	var start, next time.Time
	err := tx.QueryRow(ctx, `
		SELECT r.type, a.name, r.amount, r.category, r.title, r.freq, r.start_date, r.next_date
		FROM recurring r JOIN accounts a ON a.id = r.account_id
		WHERE r.id = $1 AND r.user_id = $2 FOR UPDATE OF r`, id, uid).
		Scan(&r.Type, &r.Account, &r.Amount, &r.Category, &r.Title, &r.Freq, &start, &next)
	if err != nil {
		return 0, err
	}
	n := 0
	var last time.Time
	for d := next; !d.After(today) && n < maxCatchUp; d = nextAfter(start, r.Freq, d) {
		in := TxInput{Date: d.Format(time.DateOnly), Title: r.Title, Type: r.Type, Category: r.Category, Account: r.Account, Amount: r.Amount}
		if err := in.validate(); err != nil {
			return n, err
		}
		if err := insertTx(ctx, tx, uid, in, nil); err != nil {
			return n, err
		}
		last, n = d, n+1
	}
	if n == 0 {
		return 0, nil
	}
	_, err = tx.Exec(ctx, `UPDATE recurring SET last_date = $2, next_date = $3 WHERE id = $1`,
		id, last, nextAfter(start, r.Freq, last))
	return n, err
}

// ApplyDueRecurring создаёт операции всех правил, чей срок наступил. Каждое
// правило — своей транзакцией: ошибка в одном не держит остальные.
func (s *Store) ApplyDueRecurring(ctx context.Context, log *slog.Logger, after func(uid int64)) {
	today := moscowToday()
	rows, _ := s.db.Query(ctx, `SELECT id, user_id FROM recurring WHERE next_date <= $1 ORDER BY id`, today)
	due, err := pgx.CollectRows(rows, pgx.RowToStructByPos[struct{ ID, UID int64 }])
	if err != nil {
		log.Warn("recurring: list due", "err", err)
		return
	}
	for _, r := range due {
		var n int
		err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
			var err error
			n, err = applyRecurring(ctx, tx, r.UID, r.ID, today)
			return err
		})
		if err == nil && n > 0 {
			err = s.DataChanged(ctx, r.UID)
		}
		if err != nil {
			log.Warn("recurring: apply", "id", r.ID, "user", r.UID, "err", err)
			continue
		}
		log.Info("recurring: created operations", "id", r.ID, "user", r.UID, "count", n)
		if n > 0 && after != nil {
			after(r.UID)
		}
	}
}

// RunRecurring проверяет сроки при запуске и дальше с периодом every; after
// вызывается для пользователя, которому записаны операции.
func (s *Store) RunRecurring(ctx context.Context, log *slog.Logger, every time.Duration, after func(uid int64)) {
	s.ApplyDueRecurring(ctx, log, after)
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.ApplyDueRecurring(ctx, log, after)
		}
	}
}
