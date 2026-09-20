package main

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"log/slog"
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

// defaultCategories are copied to every new user.
var defaultCategories = []struct{ name, kind string }{
	{"Продукты", "expense"},
	{"Кафе и доставка", "expense"},
	{"Транспорт", "expense"},
	{"Жильё и связь", "expense"},
	{"Здоровье", "expense"},
	{"Подписки", "expense"},
	{"Спорт", "expense"},
	{"Одежда", "expense"},
	{"Развлечения", "expense"},
	{"Накопления", "expense"},
	{"Зарплата", "income"},
	{"Подработка", "income"},
	{"Проценты по вкладу", "income"},
	{"Возврат", "income"},
}

type Account struct {
	ID      int64   `json:"id"`
	Name    string  `json:"name"`
	Kind    string  `json:"kind"`
	Balance float64 `json:"balance"`
	Cur     string  `json:"cur"`
	// CreatedAt dates the opening-balance row of the export
	CreatedAt string `json:"createdAt"`
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
	Time   string   `json:"tm,omitempty"` // 14:30 or 14:30:05, empty when unknown
	Title  string   `json:"t"`
	Cat    string   `json:"c"`
	Acc    string   `json:"a"`
	To     string   `json:"to,omitempty"`
	V      float64  `json:"v"`
	Got    *float64 `json:"got,omitempty"`
	GotCur string   `json:"gotCur,omitempty"`
	Type   string   `json:"type"`
}

// CryptoPortfolio is a wallet or an exchange account with coins inside. The
// page counts its balance from the rates and its profit from Invested.
type CryptoPortfolio struct {
	ID     int64         `json:"id"`
	Name   string        `json:"name"`
	Place  string        `json:"place"`
	Assets []CryptoAsset `json:"assets"`
}

// CryptoAsset is one coin in a portfolio: how much of it there is and how much
// was put into it, in USDT.
type CryptoAsset struct {
	ID       int64   `json:"id"`
	Coin     string  `json:"coin"`
	Amount   float64 `json:"amount"`
	Invested float64 `json:"invested"`
}

type Rate struct {
	Rub       float64   `json:"rub"`
	Source    string    `json:"source"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// State is everything the page renders for one user.
type State struct {
	User     *User             `json:"user,omitempty"`
	Accounts []Account         `json:"accounts"`
	Cats     []Category        `json:"cats"`
	Txs      []Tx              `json:"txs"`
	Crypto   []CryptoPortfolio `json:"crypto"`
	Rates    map[string]Rate   `json:"rates"`
}

// APIError is a user-facing error: its message is shown in the page as is.
type APIError struct {
	Status int
	Msg    string
}

func (e *APIError) Error() string { return e.Msg }

func badRequest(msg string) error { return &APIError{400, msg} }
func notFound(msg string) error   { return &APIError{404, msg} }

// Store keeps each user's data apart: every method that touches accounts,
// categories or operations takes the user id and filters by it.
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

// State returns the user's data, newest operations first, and the shared rates.
func (s *Store) State(ctx context.Context, uid int64) (*State, error) {
	st := &State{Rates: map[string]Rate{}}
	var err error

	rows, _ := s.db.Query(ctx, `
		SELECT id, name, kind, balance, currency, to_char(created_at, 'YYYY-MM-DD')
		FROM accounts WHERE user_id = $1 ORDER BY id`, uid)
	st.Accounts, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (Account, error) {
		var a Account
		err := r.Scan(&a.ID, &a.Name, &a.Kind, &a.Balance, &a.Cur, &a.CreatedAt)
		return a, err
	})
	if err != nil {
		return nil, err
	}

	rows, _ = s.db.Query(ctx, `SELECT id, name, parent_id, kind FROM categories WHERE user_id = $1 ORDER BY id`, uid)
	st.Cats, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (Category, error) {
		var c Category
		err := r.Scan(&c.ID, &c.Name, &c.Parent, &c.Kind)
		return c, err
	})
	if err != nil {
		return nil, err
	}

	rows, _ = s.db.Query(ctx, `
		SELECT t.id, to_char(t.date, 'YYYY-MM-DD'),
		       CASE WHEN t.time IS NULL THEN ''
		            WHEN right(t.time::text, 3) = ':00' THEN left(t.time::text, 5)
		            ELSE t.time::text END,
		       t.title, t.type, t.category_name,
		       a.name, COALESCE(b.name, ''), COALESCE(b.currency, ''), t.amount, t.received
		FROM transactions t
		JOIN accounts a ON a.id = t.account_id
		LEFT JOIN accounts b ON b.id = t.to_account_id
		WHERE t.user_id = $1
		ORDER BY t.date DESC, t.time DESC NULLS LAST, t.id DESC`, uid)
	st.Txs, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (Tx, error) {
		var t Tx
		var received *float64
		err := r.Scan(&t.ID, &t.Date, &t.Time, &t.Title, &t.Type, &t.Cat, &t.Acc, &t.To, &t.GotCur, &t.V, &received)
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

	st.Crypto, err = s.cryptoPortfolios(ctx, uid)
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

func (s *Store) CreateAccount(ctx context.Context, uid int64, in AccountInput) error {
	if err := in.validate(); err != nil {
		return err
	}
	_, err := s.db.Exec(ctx, `INSERT INTO accounts (user_id, name, kind, balance, currency) VALUES ($1, $2, $3, $4, $5)`,
		uid, in.Name, in.Kind, in.Balance, in.Cur)
	return err
}

// UpdateAccount sets the balance directly, like the page's account dialog.
func (s *Store) UpdateAccount(ctx context.Context, uid, id int64, in AccountInput) error {
	if err := in.validate(); err != nil {
		return err
	}
	tag, err := s.db.Exec(ctx, `
		UPDATE accounts SET name = $3, kind = $4, balance = $5, currency = $6
		WHERE id = $1 AND user_id = $2`,
		id, uid, in.Name, in.Kind, in.Balance, in.Cur)
	if err == nil && tag.RowsAffected() == 0 {
		return notFound("Счёт не найден")
	}
	return err
}

// DeleteAccount removes the account. With replace > 0 its operations move to
// that account — the currency must be the same — and it takes over the deleted
// balance; transfers between the two would become transfers to themselves, so
// they go. Without a replacement the operations are deleted with the account.
// Either way the balances of the other accounts lose what the gone operations
// had added to them.
func (s *Store) DeleteAccount(ctx context.Context, uid, id, replace int64) error {
	return pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		var cur string
		err := tx.QueryRow(ctx, `SELECT currency FROM accounts WHERE id = $1 AND user_id = $2 FOR UPDATE`, id, uid).Scan(&cur)
		if errors.Is(err, pgx.ErrNoRows) {
			return notFound("Счёт не найден")
		}
		if err != nil {
			return err
		}
		if replace > 0 {
			if err := moveTxs(ctx, tx, uid, id, replace, cur); err != nil {
				return err
			}
		} else if err := undoTxs(ctx, tx, uid, id); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `DELETE FROM accounts WHERE id = $1 AND user_id = $2`, id, uid)
		if err == nil && tag.RowsAffected() == 0 {
			return notFound("Счёт не найден")
		}
		return err
	})
}

// moveTxs hands the operations of one account over to another of the same currency.
func moveTxs(ctx context.Context, tx pgx.Tx, uid, id, replace int64, cur string) error {
	if replace == id {
		return badRequest("Перенести операции можно только на другой счёт")
	}
	var repCur string
	err := tx.QueryRow(ctx, `SELECT currency FROM accounts WHERE id = $1 AND user_id = $2 FOR UPDATE`, replace, uid).Scan(&repCur)
	if errors.Is(err, pgx.ErrNoRows) {
		return notFound("Счёт для переноса не найден")
	}
	if err != nil {
		return err
	}
	if repCur != cur {
		return badRequest("Операции можно перенести только на счёт в той же валюте")
	}

	rows, err := tx.Query(ctx, `
		SELECT id FROM transactions
		WHERE user_id = $1 AND ((account_id = $2 AND to_account_id = $3) OR (account_id = $3 AND to_account_id = $2))`,
		uid, id, replace)
	if err != nil {
		return err
	}
	selfTransfers, err := pgx.CollectRows(rows, pgx.RowTo[int64])
	if err != nil {
		return err
	}
	for _, txID := range selfTransfers {
		if err := removeTx(ctx, tx, uid, txID); err != nil {
			return err
		}
	}

	if _, err := tx.Exec(ctx, `UPDATE transactions SET account_id = $3 WHERE user_id = $1 AND account_id = $2`, uid, id, replace); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `UPDATE transactions SET to_account_id = $3 WHERE user_id = $1 AND to_account_id = $2`, uid, id, replace); err != nil {
		return err
	}
	// вместе с операциями переезжает и остаток — в нём есть и начальный, которого нет в операциях
	var left float64
	if err := tx.QueryRow(ctx, `SELECT balance FROM accounts WHERE id = $1 AND user_id = $2`, id, uid).Scan(&left); err != nil {
		return err
	}
	return addBalance(ctx, tx, replace, left)
}

// undoTxs takes the operations of the account being deleted out of the balances
// of the accounts on the other side of them. The operations themselves go with
// the account, by the foreign key.
func undoTxs(ctx context.Context, tx pgx.Tx, uid, id int64) error {
	if _, err := tx.Exec(ctx, `
		UPDATE accounts a SET balance = a.balance - x.total
		FROM (SELECT to_account_id AS id, SUM(received) AS total FROM transactions
			WHERE user_id = $1 AND account_id = $2 AND to_account_id IS NOT NULL
			GROUP BY to_account_id) x
		WHERE a.id = x.id AND a.user_id = $1`, uid, id); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `
		UPDATE accounts a SET balance = a.balance + x.total
		FROM (SELECT account_id AS id, SUM(amount) AS total FROM transactions
			WHERE user_id = $1 AND to_account_id = $2
			GROUP BY account_id) x
		WHERE a.id = x.id AND a.user_id = $1`, uid, id)
	return err
}

// ---- operations ----

type TxInput struct {
	Date      string   `json:"date"`
	Time      string   `json:"time"` // optional: 14:30 or 14:30:05
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
	if in.Time != "" {
		clock, ok := parseClock(in.Time)
		if !ok {
			return badRequest("Время должно быть в формате ЧЧ:ММ или ЧЧ:ММ:СС")
		}
		in.Time = clock
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

func (s *Store) CreateTx(ctx context.Context, uid int64, in TxInput) error {
	if err := in.validate(); err != nil {
		return err
	}
	return pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error { return insertTx(ctx, tx, uid, in, nil) })
}

// UpdateTx replaces an operation: the old one is rolled back from the balances
// and the new one recorded under the same id, so its place in the list holds.
func (s *Store) UpdateTx(ctx context.Context, uid, id int64, in TxInput) error {
	if err := in.validate(); err != nil {
		return err
	}
	return pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		if err := removeTx(ctx, tx, uid, id); err != nil {
			return err
		}
		return insertTx(ctx, tx, uid, in, &id)
	})
}

// insertTx records a validated operation and moves the account balances; id
// is nil for a new one. A transfer without an explicit received amount is
// converted at the stored rates.
func insertTx(ctx context.Context, tx pgx.Tx, uid int64, in TxInput, id *int64) error {
	from, err := lockAccount(ctx, tx, uid, in.Account)
	if err != nil {
		return err
	}

	if in.Type == "transfer" {
		to, err := lockAccount(ctx, tx, uid, in.ToAccount)
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
			INSERT INTO transactions (id, user_id, date, title, type, account_id, to_account_id, amount, received, time)
			VALUES (COALESCE($9::bigint, nextval(pg_get_serial_sequence('transactions', 'id'))), $1, $2, $3, 'transfer', $4, $5, $6, $7, NULLIF($8, '')::time)`,
			uid, in.Date, in.Title, from.id, to.id, in.Amount, got, in.Time, id); err != nil {
			return err
		}
		if err := addBalance(ctx, tx, from.id, -in.Amount); err != nil {
			return err
		}
		return addBalance(ctx, tx, to.id, got)
	}

	var catID *int64
	var cid int64
	switch err := tx.QueryRow(ctx, `SELECT id FROM categories WHERE user_id = $1 AND name = $2`, uid, in.Category).Scan(&cid); {
	case err == nil:
		catID = &cid
	case !errors.Is(err, pgx.ErrNoRows):
		return err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO transactions (id, user_id, date, title, type, category_id, category_name, account_id, amount, time)
		VALUES (COALESCE($10::bigint, nextval(pg_get_serial_sequence('transactions', 'id'))), $1, $2, $3, $4, $5, $6, $7, $8, NULLIF($9, '')::time)`,
		uid, in.Date, in.Title, in.Type, catID, in.Category, from.id, in.Amount, in.Time, id); err != nil {
		return err
	}
	delta := in.Amount
	if in.Type == "expense" {
		delta = -delta
	}
	return addBalance(ctx, tx, from.id, delta)
}

// DeleteTx removes an operation and rolls its effect back from the balances.
func (s *Store) DeleteTx(ctx context.Context, uid, id int64) error {
	return pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error { return removeTx(ctx, tx, uid, id) })
}

func removeTx(ctx context.Context, tx pgx.Tx, uid, id int64) error {
	var typ string
	var from int64
	var to *int64
	var amount float64
	var received *float64
	err := tx.QueryRow(ctx, `
			DELETE FROM transactions WHERE id = $1 AND user_id = $2
			RETURNING type, account_id, to_account_id, amount, received`, id, uid).
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
}

type accountRef struct {
	id  int64
	cur string
}

func lockAccount(ctx context.Context, tx pgx.Tx, uid int64, name string) (accountRef, error) {
	var a accountRef
	err := tx.QueryRow(ctx, `SELECT id, currency FROM accounts WHERE user_id = $1 AND name = $2 FOR UPDATE`, uid, name).Scan(&a.id, &a.cur)
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

// ---- crypto portfolios ----

// cryptoPortfolios returns the user's portfolios with their coins inside.
func (s *Store) cryptoPortfolios(ctx context.Context, uid int64) ([]CryptoPortfolio, error) {
	rows, _ := s.db.Query(ctx, `SELECT id, name, place FROM crypto_portfolios WHERE user_id = $1 ORDER BY id`, uid)
	ports, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (CryptoPortfolio, error) {
		p := CryptoPortfolio{Assets: []CryptoAsset{}}
		err := r.Scan(&p.ID, &p.Name, &p.Place)
		return p, err
	})
	if err != nil {
		return nil, err
	}
	at := make(map[int64]int, len(ports))
	for i, p := range ports {
		at[p.ID] = i
	}
	rows, _ = s.db.Query(ctx, `
		SELECT portfolio_id, id, coin, amount, invested
		FROM crypto_assets WHERE user_id = $1 ORDER BY id`, uid)
	defer rows.Close()
	for rows.Next() {
		var portfolio int64
		var a CryptoAsset
		if err := rows.Scan(&portfolio, &a.ID, &a.Coin, &a.Amount, &a.Invested); err != nil {
			return nil, err
		}
		if i, ok := at[portfolio]; ok {
			ports[i].Assets = append(ports[i].Assets, a)
		}
	}
	if ports == nil {
		ports = []CryptoPortfolio{}
	}
	return ports, rows.Err()
}

type CryptoPortfolioInput struct {
	Name  string `json:"name"`
	Place string `json:"place"`
}

func (in *CryptoPortfolioInput) validate() error {
	in.Name, in.Place = strings.TrimSpace(in.Name), strings.TrimSpace(in.Place)
	if in.Name == "" {
		return badRequest("Укажите название портфеля")
	}
	return nil
}

// CryptoAssetInput carries the portfolio only on creation; an edit keeps the
// coin where it is.
type CryptoAssetInput struct {
	Portfolio int64   `json:"portfolio"`
	Coin      string  `json:"coin"`
	Amount    float64 `json:"amount"`
	Invested  float64 `json:"invested"`
}

func (in *CryptoAssetInput) validate() error {
	in.Coin = strings.ToUpper(strings.TrimSpace(in.Coin))
	if coinIDs[in.Coin] == "" {
		return badRequest("Неизвестная монета " + in.Coin)
	}
	if in.Amount < 0 || in.Invested < 0 {
		return badRequest("Количество и вложенная сумма не могут быть отрицательными")
	}
	return nil
}

func (s *Store) CreateCryptoPortfolio(ctx context.Context, uid int64, in CryptoPortfolioInput) error {
	if err := in.validate(); err != nil {
		return err
	}
	_, err := s.db.Exec(ctx, `INSERT INTO crypto_portfolios (user_id, name, place) VALUES ($1, $2, $3)`,
		uid, in.Name, in.Place)
	return err
}

func (s *Store) UpdateCryptoPortfolio(ctx context.Context, uid, id int64, in CryptoPortfolioInput) error {
	if err := in.validate(); err != nil {
		return err
	}
	tag, err := s.db.Exec(ctx, `
		UPDATE crypto_portfolios SET name = $3, place = $4 WHERE id = $1 AND user_id = $2`,
		id, uid, in.Name, in.Place)
	if err == nil && tag.RowsAffected() == 0 {
		return notFound("Портфель не найден")
	}
	return err
}

// DeleteCryptoPortfolio removes the portfolio with the coins in it.
func (s *Store) DeleteCryptoPortfolio(ctx context.Context, uid, id int64) error {
	tag, err := s.db.Exec(ctx, `DELETE FROM crypto_portfolios WHERE id = $1 AND user_id = $2`, id, uid)
	if err == nil && tag.RowsAffected() == 0 {
		return notFound("Портфель не найден")
	}
	return err
}

func (s *Store) CreateCryptoAsset(ctx context.Context, uid int64, in CryptoAssetInput) error {
	if err := in.validate(); err != nil {
		return err
	}
	var exists bool
	if err := s.db.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM crypto_portfolios WHERE id = $1 AND user_id = $2)`,
		in.Portfolio, uid).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return notFound("Портфель не найден")
	}
	if err := s.coinIsFree(ctx, uid, in.Portfolio, in.Coin, 0); err != nil {
		return err
	}
	_, err := s.db.Exec(ctx, `
		INSERT INTO crypto_assets (user_id, portfolio_id, coin, amount, invested) VALUES ($1, $2, $3, $4, $5)`,
		uid, in.Portfolio, in.Coin, in.Amount, in.Invested)
	return err
}

func (s *Store) UpdateCryptoAsset(ctx context.Context, uid, id int64, in CryptoAssetInput) error {
	if err := in.validate(); err != nil {
		return err
	}
	var portfolio int64
	err := s.db.QueryRow(ctx, `SELECT portfolio_id FROM crypto_assets WHERE id = $1 AND user_id = $2`, id, uid).Scan(&portfolio)
	if errors.Is(err, pgx.ErrNoRows) {
		return notFound("Монета не найдена")
	}
	if err != nil {
		return err
	}
	if err := s.coinIsFree(ctx, uid, portfolio, in.Coin, id); err != nil {
		return err
	}
	_, err = s.db.Exec(ctx, `
		UPDATE crypto_assets SET coin = $3, amount = $4, invested = $5 WHERE id = $1 AND user_id = $2`,
		id, uid, in.Coin, in.Amount, in.Invested)
	return err
}

func (s *Store) DeleteCryptoAsset(ctx context.Context, uid, id int64) error {
	tag, err := s.db.Exec(ctx, `DELETE FROM crypto_assets WHERE id = $1 AND user_id = $2`, id, uid)
	if err == nil && tag.RowsAffected() == 0 {
		return notFound("Монета не найдена")
	}
	return err
}

// coinIsFree keeps one row per coin in a portfolio, so its amount is in one
// place; except is the id of the row being edited (0 when creating).
func (s *Store) coinIsFree(ctx context.Context, uid, portfolio int64, coin string, except int64) error {
	var taken bool
	err := s.db.QueryRow(ctx, `SELECT EXISTS (
		SELECT 1 FROM crypto_assets WHERE user_id = $1 AND portfolio_id = $2 AND coin = $3 AND id <> $4)`,
		uid, portfolio, coin, except).Scan(&taken)
	if err != nil {
		return err
	}
	if taken {
		return badRequest(coin + " уже есть в этом портфеле — откройте монету и измените количество")
	}
	return nil
}

// Reset empties the account: every operation, account, category and crypto
// portfolio of the user goes, and the default categories come back — the state of a fresh sign-up.
func (s *Store) Reset(ctx context.Context, uid int64) error {
	return pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		for _, q := range []string{
			`DELETE FROM transactions WHERE user_id = $1`,
			`DELETE FROM accounts WHERE user_id = $1`,
			`DELETE FROM categories WHERE user_id = $1`,
			`DELETE FROM crypto_portfolios WHERE user_id = $1`,
		} {
			if _, err := tx.Exec(ctx, q, uid); err != nil {
				return err
			}
		}
		return seedCategories(ctx, tx, uid)
	})
}

func seedCategories(ctx context.Context, tx pgx.Tx, uid int64) error {
	names := make([]string, len(defaultCategories))
	kinds := make([]string, len(defaultCategories))
	for i, c := range defaultCategories {
		names[i], kinds[i] = c.name, c.kind
	}
	_, err := tx.Exec(ctx, `INSERT INTO categories (user_id, name, kind) SELECT $1, unnest($2::text[]), unnest($3::text[])`,
		uid, names, kinds)
	return err
}

func (s *Store) CreateCategory(ctx context.Context, uid int64, in CategoryInput) error {
	if err := in.validate(); err != nil {
		return err
	}
	return pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		if err := checkParent(ctx, tx, uid, 0, in); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `INSERT INTO categories (user_id, name, parent_id, kind) VALUES ($1, $2, $3, $4)`,
			uid, in.Name, in.Parent, in.Kind)
		return err
	})
}

// UpdateCategory renames or moves a category; its subtree takes on its kind
// and its operations take on the new name.
func (s *Store) UpdateCategory(ctx context.Context, uid, id int64, in CategoryInput) error {
	if err := in.validate(); err != nil {
		return err
	}
	return pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		if err := checkParent(ctx, tx, uid, id, in); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `UPDATE categories SET name = $3, parent_id = $4, kind = $5 WHERE id = $1 AND user_id = $2`,
			id, uid, in.Name, in.Parent, in.Kind)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return notFound("Категория не найдена")
		}
		if _, err := tx.Exec(ctx, `
			WITH RECURSIVE down AS (
				SELECT id FROM categories WHERE parent_id = $1 AND user_id = $2
				UNION ALL
				SELECT c.id FROM categories c JOIN down ON c.parent_id = down.id
			)
			UPDATE categories SET kind = $3 WHERE id IN (SELECT id FROM down)`, id, uid, in.Kind); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE transactions SET category_name = $3 WHERE category_id = $1 AND user_id = $2`, id, uid, in.Name)
		return err
	})
}

// checkParent rejects a parent that isn't the user's, a parent of another kind,
// a parent inside the category's own subtree and nesting deeper than
// maxCategoryDepth. id is 0 for a new category.
func checkParent(ctx context.Context, tx pgx.Tx, uid, id int64, in CategoryInput) error {
	if in.Parent == nil {
		return nil
	}
	var kind *string
	var parentDepth, height int
	var cyclic bool
	err := tx.QueryRow(ctx, `
		WITH RECURSIVE up AS (
			SELECT id, parent_id, kind, 0 AS depth FROM categories WHERE id = $1 AND user_id = $3
			UNION ALL
			SELECT c.id, c.parent_id, c.kind, up.depth + 1 FROM categories c JOIN up ON c.id = up.parent_id
		), down AS (
			SELECT id, 0 AS depth FROM categories WHERE id = $2 AND user_id = $3
			UNION ALL
			SELECT c.id, down.depth + 1 FROM categories c JOIN down ON c.parent_id = down.id
		)
		SELECT (SELECT kind FROM up WHERE depth = 0),
		       COALESCE((SELECT max(depth) FROM up), 0),
		       COALESCE((SELECT max(depth) FROM down), 0),
		       COALESCE((SELECT bool_or(id = $2) FROM up), false)`,
		*in.Parent, id, uid).Scan(&kind, &parentDepth, &height, &cyclic)
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
func (s *Store) DeleteCategory(ctx context.Context, uid, id int64) error {
	return pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `
			UPDATE categories SET parent_id = (SELECT parent_id FROM categories WHERE id = $1 AND user_id = $2)
			WHERE parent_id = $1 AND user_id = $2`, id, uid); err != nil {
			return err
		}
		tag, err := tx.Exec(ctx, `DELETE FROM categories WHERE id = $1 AND user_id = $2`, id, uid)
		if err == nil && tag.RowsAffected() == 0 {
			return notFound("Категория не найдена")
		}
		return err
	})
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
