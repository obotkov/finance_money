package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Бекап — полный снимок данных пользователя: счета с остатками, категории,
// операции и крипто-портфели. Хранится на сервере в таблице backups, скачивается
// тем же JSON-файлом и из него же восстанавливается. Суммы лежат строками —
// NUMERIC как есть, без округления через float.

const (
	backupFormat  = "penny-backup"
	backupVersion = 1
	// сколько бекапов хранится у пользователя: старше — удаляются при новом
	maxBackups = 30
	// предел для загружаемого файла
	maxBackupBytes = 32 << 20
)

// BackupInfo — строка списка бекапов на странице.
type BackupInfo struct {
	ID        int64     `json:"id"`
	CreatedAt time.Time `json:"createdAt"`
	Kind      string    `json:"kind"`
	Accounts  int       `json:"accounts"`
	Txs       int       `json:"txs"`
	Size      int       `json:"size"`
}

type backupFile struct {
	Format       string            `json:"format"`
	Version      int               `json:"version"`
	CreatedAt    time.Time         `json:"createdAt"`
	Accounts     []backupAccount   `json:"accounts"`
	Categories   []backupCategory  `json:"categories"`
	Transactions []backupTx        `json:"transactions"`
	Crypto       []backupPortfolio `json:"cryptoPortfolios"`
}

type backupAccount struct {
	ID        int64     `json:"id"`
	Name      string    `json:"name"`
	Kind      string    `json:"kind"`
	Currency  string    `json:"currency"`
	Balance   string    `json:"balance"`
	CreatedAt time.Time `json:"createdAt"`
}

type backupCategory struct {
	ID     int64  `json:"id"`
	Name   string `json:"name"`
	Parent *int64 `json:"parent"`
	Kind   string `json:"kind"`
	Icon   string `json:"icon"`
}

type backupTx struct {
	ID           int64     `json:"id"`
	Date         string    `json:"date"`
	Time         string    `json:"time,omitempty"`
	Title        string    `json:"title"`
	Type         string    `json:"type"`
	CategoryID   *int64    `json:"categoryId"`
	CategoryName string    `json:"categoryName"`
	AccountID    int64     `json:"accountId"`
	ToAccountID  *int64    `json:"toAccountId"`
	Amount       string    `json:"amount"`
	Received     *string   `json:"received"`
	Coin         string    `json:"coin,omitempty"`
	ClosePrice   *string   `json:"closePrice,omitempty"`
	CloseDate    string    `json:"closeDate,omitempty"`
	CreatedAt    time.Time `json:"createdAt"`
}

type backupPortfolio struct {
	Name      string        `json:"name"`
	Place     string        `json:"place"`
	CreatedAt time.Time     `json:"createdAt"`
	Assets    []backupAsset `json:"assets"`
}

type backupAsset struct {
	Coin      string    `json:"coin"`
	Amount    string    `json:"amount"`
	Invested  string    `json:"invested"`
	CreatedAt time.Time `json:"createdAt"`
}

// Backups — список бекапов пользователя, новые сверху.
func (s *Store) Backups(ctx context.Context, uid int64) ([]BackupInfo, error) {
	rows, _ := s.db.Query(ctx, `
		SELECT id, created_at, kind, accounts, txs, size FROM backups
		WHERE user_id = $1 ORDER BY created_at DESC, id DESC`, uid)
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (BackupInfo, error) {
		var b BackupInfo
		err := r.Scan(&b.ID, &b.CreatedAt, &b.Kind, &b.Accounts, &b.Txs, &b.Size)
		return b, err
	})
}

// CreateBackup сохраняет снимок текущих данных.
func (s *Store) CreateBackup(ctx context.Context, uid int64) error {
	return pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		return saveBackup(ctx, tx, uid, "manual")
	})
}

// DeleteBackup удаляет один бекап.
func (s *Store) DeleteBackup(ctx context.Context, uid, id int64) error {
	tag, err := s.db.Exec(ctx, `DELETE FROM backups WHERE id = $1 AND user_id = $2`, id, uid)
	if err == nil && tag.RowsAffected() == 0 {
		return notFound("Бекап не найден")
	}
	return err
}

// BackupJSON — файл бекапа для скачивания.
func (s *Store) BackupJSON(ctx context.Context, uid, id int64) ([]byte, time.Time, error) {
	var data []byte
	var at time.Time
	err := s.db.QueryRow(ctx, `SELECT data, created_at FROM backups WHERE id = $1 AND user_id = $2`, id, uid).Scan(&data, &at)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, at, notFound("Бекап не найден")
	}
	if err != nil {
		return nil, at, err
	}
	out, err := gunzip(data)
	return out, at, err
}

// RestoreBackup заменяет данные пользователя сохранённым бекапом.
func (s *Store) RestoreBackup(ctx context.Context, uid, id int64) error {
	raw, _, err := s.BackupJSON(ctx, uid, id)
	if err != nil {
		return err
	}
	return s.RestoreFile(ctx, uid, raw)
}

// RestoreFile заменяет данные пользователя содержимым файла бекапа. Перед этим
// текущие данные сами сохраняются бекапом «перед восстановлением», чтобы
// восстановление можно было отменить. Всё в одной транзакции: если файл не
// встал, не меняется ничего.
func (s *Store) RestoreFile(ctx context.Context, uid int64, raw []byte) error {
	f, err := parseBackup(raw)
	if err != nil {
		return err
	}
	err = pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		if err := saveBackup(ctx, tx, uid, "auto"); err != nil {
			return err
		}
		return restore(ctx, tx, uid, f)
	})
	// нарушенные ограничения и кривые значения — это файл, а не сервер
	var pe *pgconn.PgError
	if errors.As(err, &pe) && pe.Code == "23505" {
		return badRequest("В бекапе повторяются названия счетов, категорий или портфелей")
	}
	if errors.As(err, &pe) && (strings.HasPrefix(pe.Code, "22") || strings.HasPrefix(pe.Code, "23")) {
		return badRequest("Файл бекапа повреждён: " + pe.Message)
	}
	return err
}

// parseBackup читает файл и проверяет то, что не проверит база: формат,
// валюты счетов, ссылки между записями и отсутствие циклов в категориях.
func parseBackup(raw []byte) (*backupFile, error) {
	var f backupFile
	if err := json.Unmarshal(raw, &f); err != nil || f.Format != backupFormat {
		return nil, badRequest("Это не файл бекапа Penny")
	}
	if f.Version != backupVersion {
		return nil, badRequest(fmt.Sprintf("Версия бекапа %d не поддерживается", f.Version))
	}
	accs := map[int64]bool{}
	for _, a := range f.Accounts {
		if strings.TrimSpace(a.Name) == "" {
			return nil, badRequest("В бекапе счёт без названия")
		}
		if !currencies[a.Currency] {
			return nil, badRequest("В бекапе неизвестная валюта " + a.Currency)
		}
		accs[a.ID] = true
	}
	parent := map[int64]*int64{}
	for _, c := range f.Categories {
		parent[c.ID] = c.Parent
	}
	for _, c := range f.Categories {
		depth := 0
		for p := c.Parent; p != nil; p = parent[*p] {
			if _, ok := parent[*p]; !ok {
				return nil, badRequest("В бекапе у категории «" + c.Name + "» нет родителя")
			}
			if depth++; depth > maxCategoryDepth {
				return nil, badRequest("В бекапе слишком глубокая вложенность категорий у «" + c.Name + "»")
			}
		}
	}
	for _, t := range f.Transactions {
		if !accs[t.AccountID] || (t.ToAccountID != nil && !accs[*t.ToAccountID]) {
			return nil, badRequest("В бекапе операция «" + t.Title + "» ссылается на счёт, которого нет")
		}
	}
	return &f, nil
}

// saveBackup снимает данные пользователя и кладёт их в backups, оставляя
// последние maxBackups. Пустой аккаунт перед восстановлением не сохраняется —
// возвращать там нечего.
func saveBackup(ctx context.Context, tx pgx.Tx, uid int64, kind string) error {
	f, err := dumpBackup(ctx, tx, uid)
	if err != nil {
		return err
	}
	if kind == "auto" && len(f.Accounts) == 0 && len(f.Transactions) == 0 && len(f.Crypto) == 0 {
		return nil
	}
	raw, err := json.Marshal(f)
	if err != nil {
		return err
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(raw); err != nil {
		return err
	}
	if err := zw.Close(); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO backups (user_id, created_at, kind, accounts, txs, size, data) VALUES ($1, $2, $3, $4, $5, $6, $7)`,
		uid, f.CreatedAt, kind, len(f.Accounts), len(f.Transactions), len(raw), buf.Bytes()); err != nil {
		return err
	}
	_, err = tx.Exec(ctx, `
		DELETE FROM backups WHERE user_id = $1 AND id NOT IN (
			SELECT id FROM backups WHERE user_id = $1 ORDER BY created_at DESC, id DESC LIMIT $2)`, uid, maxBackups)
	return err
}

func dumpBackup(ctx context.Context, tx pgx.Tx, uid int64) (*backupFile, error) {
	f := &backupFile{Format: backupFormat, Version: backupVersion, CreatedAt: time.Now().UTC().Truncate(time.Second)}
	var err error

	rows, _ := tx.Query(ctx, `
		SELECT id, name, kind, currency, balance::text, created_at
		FROM accounts WHERE user_id = $1 ORDER BY id`, uid)
	f.Accounts, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (backupAccount, error) {
		var a backupAccount
		err := r.Scan(&a.ID, &a.Name, &a.Kind, &a.Currency, &a.Balance, &a.CreatedAt)
		return a, err
	})
	if err != nil {
		return nil, err
	}

	rows, _ = tx.Query(ctx, `SELECT id, name, parent_id, kind, icon FROM categories WHERE user_id = $1 ORDER BY id`, uid)
	f.Categories, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (backupCategory, error) {
		var c backupCategory
		err := r.Scan(&c.ID, &c.Name, &c.Parent, &c.Kind, &c.Icon)
		return c, err
	})
	if err != nil {
		return nil, err
	}

	rows, _ = tx.Query(ctx, `
		SELECT id, to_char(date, 'YYYY-MM-DD'), COALESCE(time::text, ''), title, type, category_id, category_name,
		       account_id, to_account_id, amount::text, received::text, coin, close_price::text,
		       COALESCE(to_char(close_date, 'YYYY-MM-DD'), ''), created_at
		FROM transactions WHERE user_id = $1 ORDER BY date, time NULLS FIRST, id`, uid)
	f.Transactions, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (backupTx, error) {
		var t backupTx
		err := r.Scan(&t.ID, &t.Date, &t.Time, &t.Title, &t.Type, &t.CategoryID, &t.CategoryName,
			&t.AccountID, &t.ToAccountID, &t.Amount, &t.Received, &t.Coin, &t.ClosePrice, &t.CloseDate, &t.CreatedAt)
		return t, err
	})
	if err != nil {
		return nil, err
	}

	rows, _ = tx.Query(ctx, `SELECT id, name, place, created_at FROM crypto_portfolios WHERE user_id = $1 ORDER BY id`, uid)
	var ids []int64
	f.Crypto, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (backupPortfolio, error) {
		var p backupPortfolio
		var id int64
		err := r.Scan(&id, &p.Name, &p.Place, &p.CreatedAt)
		ids = append(ids, id)
		p.Assets = []backupAsset{}
		return p, err
	})
	if err != nil {
		return nil, err
	}
	for i, id := range ids {
		rows, _ = tx.Query(ctx, `
			SELECT coin, amount::text, invested::text, created_at FROM crypto_assets
			WHERE user_id = $1 AND portfolio_id = $2 ORDER BY id`, uid, id)
		f.Crypto[i].Assets, err = pgx.CollectRows(rows, func(r pgx.CollectableRow) (backupAsset, error) {
			var a backupAsset
			err := r.Scan(&a.Coin, &a.Amount, &a.Invested, &a.CreatedAt)
			return a, err
		})
		if err != nil {
			return nil, err
		}
	}
	return f, nil
}

// restore стирает данные пользователя и записывает бекап. У записей новые id,
// ссылки между ними переводятся через старые id из файла. Остатки счетов
// берутся из бекапа как есть — операции их не пересчитывают.
func restore(ctx context.Context, tx pgx.Tx, uid int64, f *backupFile) error {
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
	at := func(t time.Time) *time.Time {
		if t.IsZero() {
			return nil
		}
		return &t
	}

	cats := map[int64]int64{}
	for _, c := range f.Categories {
		var id int64
		if err := tx.QueryRow(ctx, `INSERT INTO categories (user_id, name, kind, icon) VALUES ($1, $2, $3, $4) RETURNING id`,
			uid, c.Name, c.Kind, c.Icon).Scan(&id); err != nil {
			return err
		}
		cats[c.ID] = id
	}
	for _, c := range f.Categories {
		if c.Parent != nil {
			if _, err := tx.Exec(ctx, `UPDATE categories SET parent_id = $1 WHERE id = $2`, cats[*c.Parent], cats[c.ID]); err != nil {
				return err
			}
		}
	}

	accs := map[int64]int64{}
	for _, a := range f.Accounts {
		var id int64
		if err := tx.QueryRow(ctx, `
			INSERT INTO accounts (user_id, name, kind, currency, balance, created_at)
			VALUES ($1, $2, $3, $4, $5::numeric, COALESCE($6, now())) RETURNING id`,
			uid, a.Name, a.Kind, a.Currency, a.Balance, at(a.CreatedAt)).Scan(&id); err != nil {
			return err
		}
		accs[a.ID] = id
	}

	for _, t := range f.Transactions {
		var cat, to *int64
		if t.CategoryID != nil {
			if id, ok := cats[*t.CategoryID]; ok {
				cat = &id
			}
		}
		if t.ToAccountID != nil {
			id := accs[*t.ToAccountID]
			to = &id
		}
		var clock, closeDate *string
		if t.Time != "" {
			clock = &t.Time
		}
		if t.CloseDate != "" {
			closeDate = &t.CloseDate
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO transactions (user_id, date, time, title, type, category_id, category_name, account_id,
			                          to_account_id, amount, received, coin, close_price, close_date, created_at)
			VALUES ($1, $2::date, $3::time, $4, $5, $6, $7, $8, $9, $10::numeric, $11::numeric, $12,
			        $13::numeric, $14::date, COALESCE($15, now()))`,
			uid, t.Date, clock, t.Title, t.Type, cat, t.CategoryName, accs[t.AccountID],
			to, t.Amount, t.Received, t.Coin, t.ClosePrice, closeDate, at(t.CreatedAt)); err != nil {
			return err
		}
	}

	for _, p := range f.Crypto {
		var id int64
		if err := tx.QueryRow(ctx, `
			INSERT INTO crypto_portfolios (user_id, name, place, created_at) VALUES ($1, $2, $3, COALESCE($4, now())) RETURNING id`,
			uid, p.Name, p.Place, at(p.CreatedAt)).Scan(&id); err != nil {
			return err
		}
		for _, a := range p.Assets {
			if _, err := tx.Exec(ctx, `
				INSERT INTO crypto_assets (user_id, portfolio_id, coin, amount, invested, created_at)
				VALUES ($1, $2, $3, $4::numeric, $5::numeric, COALESCE($6, now()))`,
				uid, id, a.Coin, a.Amount, a.Invested, at(a.CreatedAt)); err != nil {
				return err
			}
		}
	}
	return nil
}

func gunzip(data []byte) ([]byte, error) {
	zr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	return io.ReadAll(zr)
}
