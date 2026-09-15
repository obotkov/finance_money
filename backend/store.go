package main

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrations embed.FS

// transferCategory is what the page shows in the category column of a transfer.
const transferCategory = "Перевод"

// maxCategoryDepth limits nesting to four levels (depth 0..3), as the page does.
const maxCategoryDepth = 3

var currencies = map[string]bool{"RUB": true, "USD": true, "EUR": true, "USDT": true, "BTC": true, "ETH": true, "TON": true}

type Account struct {
	ID      int64   `json:"id"`
	Name    string  `json:"name"`
	Kind    string  `json:"kind"`
	Balance float64 `json:"balance"`
	Cur     string  `json:"cur"`
}

type Category struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	Parent *int64 `json:"parent"`
	Kind   string `json:"kind"`
}

// Tx is an operation in the shape the page works with: v is signed for income
// and expense; a transfer carries the credited amount in got/gotCur.
type Tx struct {
	ID     int64    `json:"id"`
	Date   string   `json:"d"`
	Title  string   `json:"t"`
	Cat    string   `json:"c"`
	Acc    string   `json:"a"`
	To     string   `json:"to,omitempty"`
	V      float64  `json:"v"`
	Got    *float64 `json:"got,omitempty"`
	GotCur string   `json:"gotCur,omitempty"`
	Type   string   `json:"type"`
}

type Rate struct {
	Rub       float64   `json:"rub"`
	Source    string    `json:"source"`
	UpdatedAt time.Time `json:"updatedAt"`
}

type State struct {
	Accounts []Account       `json:"accounts"`
	Cats     []Category      `json:"cats"`
	Txs      []Tx            `json:"txs"`
	Rates    map[string]Rate `json:"rates"`
}

// APIError is a user-facing error: its message is shown in the page as is.
type APIError struct {
	Status int
	Msg    string
}

func (e *APIError) Error() string { return e.Msg }

func badRequest(msg string) error { return &APIError{400, msg} }
func notFound(msg string) error   { return &APIError{404, msg} }

type Store struct {
	db  *pgxpool.Pool
	log *slog.Logger
}

// OpenStore connects to Postgres, waiting for it to come up, and applies pending migrations.
func OpenStore(ctx context.Context, dsn string, log *slog.Logger) (*Store, error) {
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, fmt.Errorf("parse DATABASE_URL: %w", err)
	}
	for attempt := 1; ; attempt++ {
		if err = pool.Ping(ctx); err == nil {
			break
		}
		if attempt == 30 {
			pool.Close()
			return nil, fmt.Errorf("connect to database: %w", err)
		}
		log.Warn("waiting for database", "err", err)
		select {
		case <-ctx.Done():
			pool.Close()
			return nil, ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
	s := &Store{db: pool, log: log}
	if err := s.migrate(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return s, nil
}

func (s *Store) Close()                         { s.db.Close() }
func (s *Store) Ping(ctx context.Context) error { return s.db.Ping(ctx) }

func (s *Store) migrate(ctx context.Context) error {
	if _, err := s.db.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		name       TEXT PRIMARY KEY,
		applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
	)`); err != nil {
		return err
	}
	entries, err := migrations.ReadDir("migrations")
	if err != nil {
		return err
	}
	for _, e := range entries {
		var done bool
		if err := s.db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE name = $1)`, e.Name()).Scan(&done); err != nil {
			return err
		}
		if done {
			continue
		}
		sql, err := migrations.ReadFile("migrations/" + e.Name())
		if err != nil {
			return err
		}
		err = pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
			if _, err := tx.Exec(ctx, string(sql)); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `INSERT INTO schema_migrations (name) VALUES ($1)`, e.Name())
			return err
		})
		if err != nil {
			return fmt.Errorf("migration %s: %w", e.Name(), err)
		}
		s.log.Info("applied migration", "name", e.Name())
	}
	return nil
}

// State returns everything the page renders, newest operations first.
func (s *Store) State(ctx context.Context) (*State, error) {
	st := &State{Rates: map[string]Rate{}}
	var err error

	rows, _ := s.db.Query(ctx, `SELECT id, name, kind, balance, currency FROM accounts ORDER BY id`)
	st.Accounts, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (Account, error) {
		var a Account
		err := r.Scan(&a.ID, &a.Name, &a.Kind, &a.Balance, &a.Cur)
		return a, err
	})
	if err != nil {
		return nil, err
	}

	rows, _ = s.db.Query(ctx, `SELECT id, name, parent_id, kind FROM categories ORDER BY id`)
	st.Cats, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (Category, error) {
		var c Category
		err := r.Scan(&c.ID, &c.Name, &c.Parent, &c.Kind)
		return c, err
	})
	if err != nil {
		return nil, err
	}

	rows, _ = s.db.Query(ctx, `
		SELECT t.id, to_char(t.date, 'YYYY-MM-DD'), t.title, t.type, t.category_name,
		       a.name, COALESCE(b.name, ''), COALESCE(b.currency, ''), t.amount, t.received
		FROM transactions t
		JOIN accounts a ON a.id = t.account_id
		LEFT JOIN accounts b ON b.id = t.to_account_id
		ORDER BY t.date DESC, t.id DESC`)
	st.Txs, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (Tx, error) {
		var t Tx
		var received *float64
		err := r.Scan(&t.ID, &t.Date, &t.Title, &t.Type, &t.Cat, &t.Acc, &t.To, &t.GotCur, &t.V, &received)
		switch t.Type {
		case "expense":
			t.V = -t.V
		case "transfer":
			t.Cat, t.Got = transferCategory, received
		}
		return t, err
	})
	if err != nil {
		return nil, err
	}

	rows, _ = s.db.Query(ctx, `SELECT code, rub, source, updated_at FROM rates`)
	defer rows.Close()
	for rows.Next() {
		var code string
		var r Rate
		if err := rows.Scan(&code, &r.Rub, &r.Source, &r.UpdatedAt); err != nil {
			return nil, err
		}
		st.Rates[code] = r
	}
	return st, rows.Err()
}

// ---- accounts ----

type AccountInput struct {
	Name    string  `json:"name"`
	Kind    string  `json:"kind"`
	Balance float64 `json:"balance"`
	Cur     string  `json:"cur"`
}

func (in *AccountInput) validate() error {
	in.Name, in.Kind = strings.TrimSpace(in.Name), strings.TrimSpace(in.Kind)
	if in.Name == "" {
		return badRequest("Укажите название счёта")
	}
	if in.Kind == "" {
		in.Kind = "Карта"
	}
	if in.Cur == "" {
		in.Cur = "RUB"
	}
	if !currencies[in.Cur] {
		return badRequest("Неизвестная валюта " + in.Cur)
	}
	return nil
}

func (s *Store) CreateAccount(ctx context.Context, in AccountInput) error {
	if err := in.validate(); err != nil {
		return err
	}
	_, err := s.db.Exec(ctx, `INSERT INTO accounts (name, kind, balance, currency) VALUES ($1, $2, $3, $4)`,
		in.Name, in.Kind, in.Balance, in.Cur)
	return err
}

// UpdateAccount sets the balance directly, like the page's account dialog.
func (s *Store) UpdateAccount(ctx context.Context, id int64, in AccountInput) error {
	if err := in.validate(); err != nil {
		return err
	}
	tag, err := s.db.Exec(ctx, `UPDATE accounts SET name = $2, kind = $3, balance = $4, currency = $5 WHERE id = $1`,
		id, in.Name, in.Kind, in.Balance, in.Cur)
	if err == nil && tag.RowsAffected() == 0 {
		return notFound("Счёт не найден")
	}
	return err
}

// DeleteAccount removes the account with its operations, transfers included.
// Balances of the other accounts are left as they are.
func (s *Store) DeleteAccount(ctx context.Context, id int64) error {
	tag, err := s.db.Exec(ctx, `DELETE FROM accounts WHERE id = $1`, id)
	if err == nil && tag.RowsAffected() == 0 {
		return notFound("Счёт не найден")
	}
	return err
}

// ---- operations ----

type TxInput struct {
	Date      string   `json:"date"`
	Title     string   `json:"title"`
	Type      string   `json:"type"`
	Category  string   `json:"category"`
	Account   string   `json:"account"`
	ToAccount string   `json:"toAccount"`
	Amount    float64  `json:"amount"`
	Received  *float64 `json:"received"`
}

func (in *TxInput) validate() error {
	in.Title, in.Category = strings.TrimSpace(in.Title), strings.TrimSpace(in.Category)
	if _, err := time.Parse(time.DateOnly, in.Date); err != nil {
		return badRequest("Дата должна быть в формате ГГГГ-ММ-ДД")
	}
	if !(in.Amount > 0) {
		return badRequest("Сумма должна быть больше нуля")
	}
	if in.Account == "" {
		return badRequest("Выберите счёт")
	}
	switch in.Type {
	case "transfer":
		if in.ToAccount == "" || in.ToAccount == in.Account {
			return badRequest("Выберите разные счета")
		}
		if in.Title == "" {
			in.Title = transferCategory
		}
	case "expense", "income":
		if in.Category == "" {
			return badRequest("Выберите категорию")
		}
		if in.Title == "" {
			in.Title = in.Category
		}
	default:
		return badRequest("Неизвестный тип операции")
	}
	return nil
}

func (s *Store) CreateTx(ctx context.Context, in TxInput) error {
	if err := in.validate(); err != nil {
		return err
	}
	return pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error { return insertTx(ctx, tx, in) })
}

// insertTx records a validated operation and moves the account balances.
// A transfer without an explicit received amount is converted at the stored rates.
func insertTx(ctx context.Context, tx pgx.Tx, in TxInput) error {
	from, err := lockAccount(ctx, tx, in.Account)
	if err != nil {
		return err
	}

	if in.Type == "transfer" {
		to, err := lockAccount(ctx, tx, in.ToAccount)
		if err != nil {
			return err
		}
		var got float64
		if in.Received != nil && *in.Received > 0 {
			got = *in.Received
		} else if got, err = convert(ctx, tx, in.Amount, from.cur, to.cur); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO transactions (date, title, type, account_id, to_account_id, amount, received)
			VALUES ($1, $2, 'transfer', $3, $4, $5, $6)`,
			in.Date, in.Title, from.id, to.id, in.Amount, got); err != nil {
			return err
		}
		if err := addBalance(ctx, tx, from.id, -in.Amount); err != nil {
			return err
		}
		return addBalance(ctx, tx, to.id, got)
	}

	var catID *int64
	var id int64
	switch err := tx.QueryRow(ctx, `SELECT id FROM categories WHERE name = $1`, in.Category).Scan(&id); {
	case err == nil:
		catID = &id
	case !errors.Is(err, pgx.ErrNoRows):
		return err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO transactions (date, title, type, category_id, category_name, account_id, amount)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		in.Date, in.Title, in.Type, catID, in.Category, from.id, in.Amount); err != nil {
		return err
	}
	delta := in.Amount
	if in.Type == "expense" {
		delta = -delta
	}
	return addBalance(ctx, tx, from.id, delta)
}

// DeleteTx removes an operation and rolls its effect back from the balances.
func (s *Store) DeleteTx(ctx context.Context, id int64) error {
	return pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		var typ string
		var from int64
		var to *int64
		var amount float64
		var received *float64
		err := tx.QueryRow(ctx, `
			DELETE FROM transactions WHERE id = $1
			RETURNING type, account_id, to_account_id, amount, received`, id).
			Scan(&typ, &from, &to, &amount, &received)
		if errors.Is(err, pgx.ErrNoRows) {
			return notFound("Операция не найдена")
		}
		if err != nil {
			return err
		}
		switch typ {
		case "transfer":
			if err := addBalance(ctx, tx, from, amount); err != nil {
				return err
			}
			return addBalance(ctx, tx, *to, -*received)
		case "income":
			return addBalance(ctx, tx, from, -amount)
		default:
			return addBalance(ctx, tx, from, amount)
		}
	})
}

type accountRef struct {
	id  int64
	cur string
}

func lockAccount(ctx context.Context, tx pgx.Tx, name string) (accountRef, error) {
	var a accountRef
	err := tx.QueryRow(ctx, `SELECT id, currency FROM accounts WHERE name = $1 FOR UPDATE`, name).Scan(&a.id, &a.cur)
	if errors.Is(err, pgx.ErrNoRows) {
		return a, badRequest("Нет счёта «" + name + "»")
	}
	return a, err
}

func addBalance(ctx context.Context, tx pgx.Tx, id int64, delta float64) error {
	_, err := tx.Exec(ctx, `UPDATE accounts SET balance = balance + $2 WHERE id = $1`, id, delta)
	return err
}

func convert(ctx context.Context, tx pgx.Tx, amount float64, from, to string) (float64, error) {
	if from == to {
		return amount, nil
	}
	rf, err := rubRate(ctx, tx, from)
	if err != nil {
		return 0, err
	}
	rt, err := rubRate(ctx, tx, to)
	if err != nil {
		return 0, err
	}
	return amount * rf / rt, nil
}

func rubRate(ctx context.Context, tx pgx.Tx, code string) (float64, error) {
	if code == "RUB" {
		return 1, nil
	}
	var rub float64
	err := tx.QueryRow(ctx, `SELECT rub FROM rates WHERE code = $1`, code).Scan(&rub)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, badRequest("Нет курса для " + code)
	}
	return rub, err
}

// ---- categories ----

type CategoryInput struct {
	Name   string `json:"name"`
	Parent *int64 `json:"parent"`
	Kind   string `json:"kind"`
}

func (in *CategoryInput) validate() error {
	in.Name = strings.TrimSpace(in.Name)
	if in.Name == "" {
		return badRequest("Укажите название категории")
	}
	if in.Name == transferCategory {
		return badRequest("«" + transferCategory + "» — служебное название, выберите другое")
	}
	if in.Kind != "expense" && in.Kind != "income" {
		return badRequest("Категория бывает только расходной или доходной")
	}
	return nil
}

func (s *Store) CreateCategory(ctx context.Context, in CategoryInput) error {
	if err := in.validate(); err != nil {
		return err
	}
	return pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		if err := checkParent(ctx, tx, 0, in); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO categories (name, parent_id, kind) VALUES ($1, $2, $3)`, in.Name, in.Parent, in.Kind)
		return err
	})
}

// UpdateCategory renames or moves a category; its subtree takes on its kind
// and its operations take on the new name.
func (s *Store) UpdateCategory(ctx context.Context, id int64, in CategoryInput) error {
	if err := in.validate(); err != nil {
		return err
	}
	return pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		if err := checkParent(ctx, tx, id, in); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `UPDATE categories SET name = $2, parent_id = $3, kind = $4 WHERE id = $1`, id, in.Name, in.Parent, in.Kind)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return notFound("Категория не найдена")
		}
		if _, err := tx.Exec(ctx, `
			WITH RECURSIVE down AS (
				SELECT id FROM categories WHERE parent_id = $1
				UNION ALL
				SELECT c.id FROM categories c JOIN down ON c.parent_id = down.id
			)
			UPDATE categories SET kind = $2 WHERE id IN (SELECT id FROM down)`, id, in.Kind); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE transactions SET category_name = $2 WHERE category_id = $1`, id, in.Name)
		return err
	})
}

// checkParent rejects a parent of another kind, a parent inside the category's
// own subtree and nesting deeper than maxCategoryDepth. id is 0 for a new category.
func checkParent(ctx context.Context, tx pgx.Tx, id int64, in CategoryInput) error {
	if in.Parent == nil {
		return nil
	}
	var kind *string
	var parentDepth, height int
	var cyclic bool
	err := tx.QueryRow(ctx, `
		WITH RECURSIVE up AS (
			SELECT id, parent_id, kind, 0 AS depth FROM categories WHERE id = $1
			UNION ALL
			SELECT c.id, c.parent_id, c.kind, up.depth + 1 FROM categories c JOIN up ON c.id = up.parent_id
		), down AS (
			SELECT id, 0 AS depth FROM categories WHERE id = $2
			UNION ALL
			SELECT c.id, down.depth + 1 FROM categories c JOIN down ON c.parent_id = down.id
		)
		SELECT (SELECT kind FROM up WHERE depth = 0),
		       COALESCE((SELECT max(depth) FROM up), 0),
		       COALESCE((SELECT max(depth) FROM down), 0),
		       COALESCE((SELECT bool_or(id = $2) FROM up), false)`,
		*in.Parent, id).Scan(&kind, &parentDepth, &height, &cyclic)
	switch {
	case err != nil:
		return err
	case kind == nil:
		return badRequest("Родительская категория не найдена")
	case cyclic:
		return badRequest("Категорию нельзя вложить в саму себя")
	case *kind != in.Kind:
		return badRequest("Родитель должен быть того же типа — доход или расход")
	case parentDepth+1+height > maxCategoryDepth:
		return badRequest("Вложенность — не больше четырёх уровней")
	}
	return nil
}

// DeleteCategory lifts the children one level up. Operations keep the category name.
func (s *Store) DeleteCategory(ctx context.Context, id int64) error {
	return pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
			UPDATE categories SET parent_id = (SELECT parent_id FROM categories WHERE id = $1)
			WHERE parent_id = $1`, id); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `DELETE FROM categories WHERE id = $1`, id)
		if err == nil && tag.RowsAffected() == 0 {
			return notFound("Категория не найдена")
		}
		return err
	})
}

// ---- CSV import ----

var (
	importSep  = regexp.MustCompile(`[;\t]`)
	importDate = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
	numberText = strings.NewReplacer(" ", "", " ", "", " ", "", "−", "-", ",", ".")
)

type importLine struct {
	n  int // line number in the pasted text
	in TxInput
}

// parseImport reads the page's CSV export format:
// дата;назначение;категория;счёт;сумма[;валюта;тип;на счёт].
// Lines that don't fit, the header among them, are skipped.
func parseImport(text string) []importLine {
	var out []importLine
	for i, line := range strings.Split(text, "\n") {
		p := importSep.Split(strings.TrimSpace(line), -1)
		for j := range p {
			p[j] = strings.TrimSpace(p[j])
		}
		if len(p) < 5 || !importDate.MatchString(p[0]) {
			continue
		}
		v, err := strconv.ParseFloat(numberText.Replace(p[4]), 64)
		if err != nil || v == 0 {
			continue
		}
		in := TxInput{Date: p[0], Title: p[1], Category: p[2], Account: p[3], Amount: math.Abs(v)}
		switch {
		case len(p) > 6 && strings.EqualFold(p[6], "перевод"):
			in.Type, in.Category = "transfer", ""
			if len(p) > 7 {
				in.ToAccount = p[7]
			}
		case v > 0:
			in.Type = "income"
		default:
			in.Type = "expense"
		}
		out = append(out, importLine{i + 1, in})
	}
	return out
}

// Import adds the operations from CSV text in one transaction and returns how
// many were added. Unknown accounts (rouble cards) and categories are created.
func (s *Store) Import(ctx context.Context, text string) (int, error) {
	lines := parseImport(text)
	if len(lines) == 0 {
		return 0, badRequest("Не найдено ни одной строки в нужном формате.")
	}
	err := pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		for _, l := range lines {
			if err := importOne(ctx, tx, l.in); err != nil {
				var ae *APIError
				if errors.As(err, &ae) {
					return badRequest(fmt.Sprintf("Строка %d: %s", l.n, ae.Msg))
				}
				return err
			}
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return len(lines), nil
}

func importOne(ctx context.Context, tx pgx.Tx, in TxInput) error {
	if err := in.validate(); err != nil {
		return err
	}
	for _, name := range []string{in.Account, in.ToAccount} {
		if name == "" {
			continue
		}
		if _, err := tx.Exec(ctx, `INSERT INTO accounts (name, kind, currency) VALUES ($1, 'Карта', 'RUB') ON CONFLICT (name) DO NOTHING`, name); err != nil {
			return err
		}
	}
	if in.Type != "transfer" {
		kind := "expense"
		if in.Type == "income" {
			kind = "income"
		}
		if _, err := tx.Exec(ctx, `INSERT INTO categories (name, kind) VALUES ($1, $2) ON CONFLICT (name) DO NOTHING`, in.Category, kind); err != nil {
			return err
		}
	}
	return insertTx(ctx, tx, in)
}

// ---- rates ----

// UpsertRates stores rub prices per currency code from one source.
func (s *Store) UpsertRates(ctx context.Context, rates map[string]float64, source string) error {
	batch := &pgx.Batch{}
	for code, rub := range rates {
		batch.Queue(`
			INSERT INTO rates (code, rub, source, updated_at) VALUES ($1, $2, $3, now())
			ON CONFLICT (code) DO UPDATE SET rub = EXCLUDED.rub, source = EXCLUDED.source, updated_at = EXCLUDED.updated_at`,
			code, rub, source)
	}
	return s.db.SendBatch(ctx, batch).Close()
}
