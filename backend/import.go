package main

import (
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"math"
	"strconv"
	"strings"
	"time"
	"unicode"

	"github.com/jackc/pgx/v5"
)

// Operations CSV, one per line — the same for the page's export and for import:
//
//	Дата;Сумма (откуда);Счёт (откуда);Сумма (куда);Счёт (куда);Баланс;Категория;Описание;Тип
//
// An expense fills the "откуда" side, income the "куда" side, a transfer both
// (its "куда" amount is in the target account's currency). Категория is a path,
// "Спорт:Футбол". Тип is расход, доход, перевод or остаток; when empty it
// follows from the filled sides. The delimiter is ";" or a tab (a paste from a
// spreadsheet).
//
// Остаток is an account's opening balance: the account in «Счёт (куда)», the
// signed amount in «Баланс». It adds to the balance without an operation, so
// it shows up in no expense or income. In other rows Баланс is ignored:
// balances follow from the operations.
const (
	colDate = iota
	colFromAmount
	colFromAccount
	colToAmount
	colToAccount
	colBalance
	colCategory
	colTitle
	colType
)

// uncategorized takes imported income and expenses that have no category.
const uncategorized = "Без категории"

type importLine struct {
	n       int      // line number in the pasted text
	in      TxInput  // Category holds the last element of catPath
	catPath []string // root first; empty for transfers
}

// parseImport reads the operations. Lines whose first field isn't a date — the
// header, notes — are skipped; a line with a date that can't be read is an error.
func parseImport(text string) ([]importLine, error) {
	r := csv.NewReader(strings.NewReader(text))
	r.Comma = importDelimiter(text)
	r.FieldsPerRecord = -1
	r.LazyQuotes = true
	// csv counts a tab as leading space and would swallow empty tab-separated
	// fields; fields are trimmed below anyway.
	r.TrimLeadingSpace = r.Comma != '\t'

	var out []importLine
	for {
		rec, err := r.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, badRequest("Не удалось разобрать CSV: " + err.Error())
		}
		n, _ := r.FieldPos(0)
		field := func(i int) string {
			if i < len(rec) {
				return strings.TrimSpace(rec[i])
			}
			return ""
		}
		date, clock, ok, err := parseImportDate(field(colDate))
		if !ok {
			continue
		}
		if err != nil {
			return nil, badRequest(fmt.Sprintf("Строка %d: %s", n, err))
		}
		l, err := parseOperation(date, field)
		if err != nil {
			return nil, badRequest(fmt.Sprintf("Строка %d: %s", n, err))
		}
		l.n, l.in.Time = n, clock
		out = append(out, l)
	}
	return out, nil
}

// importDelimiter is a tab when the first line has tabs and no semicolons.
func importDelimiter(text string) rune {
	first, _, _ := strings.Cut(strings.TrimLeft(text, "\r\n"), "\n")
	if strings.Contains(first, "\t") && !strings.Contains(first, ";") {
		return '\t'
	}
	return ';'
}

// parseImportDate reads the Дата field: 2026-09-15 or 15.09.2026 (5.9.2026),
// optionally with a time — "2026-09-15 14:30", "15.09.2026 14:30:05",
// "2026-09-15T14:30". ok is false when the field isn't a date at all (a
// header, a note); err is set for a date whose time can't be read.
func parseImportDate(s string) (date, clock string, ok bool, err error) {
	datePart, timePart, _ := strings.Cut(strings.TrimSpace(strings.Replace(s, "T", " ", 1)), " ")
	for _, layout := range []string{time.DateOnly, "2.1.2006"} {
		if t, e := time.Parse(layout, datePart); e == nil {
			date, ok = t.Format(time.DateOnly), true
			break
		}
	}
	if !ok {
		return "", "", false, nil
	}
	if timePart = strings.TrimSpace(timePart); timePart != "" {
		var valid bool
		if clock, valid = parseClock(timePart); !valid {
			return date, "", true, fmt.Errorf("время «%s»: ожидается ЧЧ:ММ или ЧЧ:ММ:СС", timePart)
		}
	}
	return date, clock, true, nil
}

// parseClock reads 14:30, 9:05 or 14:30:05 and returns it as 14:30:05.
func parseClock(s string) (string, bool) {
	for _, layout := range []string{"15:04:05", "15:04"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.Format("15:04:05"), true
		}
	}
	return "", false
}

func parseOperation(date string, field func(int) string) (importLine, error) {
	fromAcc, toAcc := field(colFromAccount), field(colToAccount)
	fromAmt, err := parseAmount(field(colFromAmount))
	if err != nil {
		return importLine{}, fmt.Errorf("«Сумма (откуда)»: %w", err)
	}
	toAmt, err := parseAmount(field(colToAmount))
	if err != nil {
		return importLine{}, fmt.Errorf("«Сумма (куда)»: %w", err)
	}
	typ, err := importType(field(colType), fromAcc, toAcc)
	if err != nil {
		return importLine{}, err
	}

	l := importLine{in: TxInput{Date: date, Title: field(colTitle), Type: typ}}
	switch typ {
	case "opening":
		acc := toAcc
		if acc == "" {
			acc = fromAcc
		}
		if acc == "" || field(colBalance) == "" {
			return l, errors.New("у остатка заполните «Счёт (куда)» и «Баланс»")
		}
		value, err := parseNumber(field(colBalance))
		if err != nil {
			return l, fmt.Errorf("«Баланс»: %w", err)
		}
		l.in.Account, l.in.Amount = acc, value
		return l, nil
	case "expense":
		if fromAcc == "" || fromAmt == 0 {
			return l, errors.New("у расхода заполните «Сумма (откуда)» и «Счёт (откуда)»")
		}
		l.in.Account, l.in.Amount = fromAcc, fromAmt
	case "income":
		if toAcc == "" || toAmt == 0 {
			return l, errors.New("у дохода заполните «Сумма (куда)» и «Счёт (куда)»")
		}
		l.in.Account, l.in.Amount = toAcc, toAmt
	case "transfer":
		if fromAcc == "" || toAcc == "" || fromAmt == 0 {
			return l, errors.New("у перевода заполните оба счёта и «Сумма (откуда)»")
		}
		l.in.Account, l.in.ToAccount, l.in.Amount = fromAcc, toAcc, fromAmt
		if toAmt > 0 {
			l.in.Received = &toAmt
		}
		return l, nil
	}

	l.catPath = categoryPath(field(colCategory))
	if len(l.catPath) == 0 {
		l.catPath = []string{uncategorized}
	}
	if len(l.catPath) > maxCategoryDepth+1 {
		return l, errors.New("категория: вложенность — не больше четырёх уровней")
	}
	l.in.Category = l.catPath[len(l.catPath)-1]
	return l, nil
}

// importType reads Тип, or infers it from which accounts are filled.
func importType(s, fromAcc, toAcc string) (string, error) {
	switch strings.ToLower(s) {
	case "расход", "expense":
		return "expense", nil
	case "доход", "income":
		return "income", nil
	case "перевод", "transfer":
		return "transfer", nil
	case "остаток", "opening":
		return "opening", nil
	case "":
		switch {
		case fromAcc != "" && toAcc != "":
			return "transfer", nil
		case fromAcc != "":
			return "expense", nil
		case toAcc != "":
			return "income", nil
		}
		return "", errors.New("не указаны ни тип, ни счета")
	}
	return "", fmt.Errorf("тип «%s»: ожидается расход, доход или перевод", s)
}

// parseAmount reads an amount as a positive number — the direction comes from
// the columns. An empty field is zero.
func parseAmount(s string) (float64, error) {
	v, err := parseNumber(s)
	return math.Abs(v), err
}

// parseNumber reads "1 234,56", "1234.56", "−620" or "1 000 ₽", keeping the
// sign. An empty field is zero.
func parseNumber(s string) (float64, error) {
	if s == "" {
		return 0, nil
	}
	clean := strings.Map(func(r rune) rune {
		switch {
		case r >= '0' && r <= '9', r == '.', r == '-':
			return r
		case r == ',':
			return '.'
		case r == '−':
			return '-'
		case unicode.IsSpace(r):
			return -1
		}
		return -1 // currency signs and other text
	}, s)
	v, err := strconv.ParseFloat(clean, 64)
	if err != nil {
		return 0, fmt.Errorf("не число: «%s»", s)
	}
	return v, nil
}

// categoryPath splits "Спорт : Футбол" into its non-empty parts.
func categoryPath(s string) []string {
	var path []string
	for _, part := range strings.Split(s, ":") {
		if part = strings.TrimSpace(part); part != "" {
			path = append(path, part)
		}
	}
	return path
}

// Import adds the operations from CSV text in one transaction and returns how
// many were added. Unknown accounts (rouble cards) and categories are created.
func (s *Store) Import(ctx context.Context, uid int64, text string) (int, error) {
	lines, err := parseImport(text)
	if err != nil {
		return 0, err
	}
	if len(lines) == 0 {
		return 0, badRequest("Не найдено ни одной операции: первая колонка — дата, 2026-09-15 или 15.09.2026.")
	}
	lines = chronological(lines)
	err = pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		for _, l := range lines {
			if err := importOne(ctx, tx, uid, l); err != nil {
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

// chronological puts a newest-first file (the export's order) oldest first, so
// that operations are stored in the order they happened: operations of a day
// without a time keep their order, accounts are created in order of first use.
// A file that isn't clearly newest-first is left as it is.
func chronological(lines []importLine) []importLine {
	if len(lines) < 2 || lines[0].in.Date <= lines[len(lines)-1].in.Date {
		return lines
	}
	out := make([]importLine, len(lines))
	for i, l := range lines {
		out[len(lines)-1-i] = l
	}
	return out
}

func importOne(ctx context.Context, tx pgx.Tx, uid int64, l importLine) error {
	in := l.in
	if in.Type == "opening" {
		if _, err := tx.Exec(ctx, `
			INSERT INTO accounts (user_id, name, kind, currency) VALUES ($1, $2, 'Карта', 'RUB')
			ON CONFLICT (user_id, name) DO NOTHING`, uid, in.Account); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `UPDATE accounts SET balance = balance + $3 WHERE user_id = $1 AND name = $2`,
			uid, in.Account, in.Amount)
		return err
	}
	if err := in.validate(); err != nil {
		return err
	}
	for _, name := range []string{in.Account, in.ToAccount} {
		if name == "" {
			continue
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO accounts (user_id, name, kind, currency) VALUES ($1, $2, 'Карта', 'RUB')
			ON CONFLICT (user_id, name) DO NOTHING`, uid, name); err != nil {
			return err
		}
	}
	if in.Type != "transfer" {
		kind := "expense"
		if in.Type == "income" {
			kind = "income"
		}
		if err := ensureCategoryPath(ctx, tx, uid, l.catPath, kind); err != nil {
			return err
		}
	}
	return insertTx(ctx, tx, uid, in)
}

// ensureCategoryPath creates the missing categories of a path, each under the
// one before it. Categories that already exist stay where they are.
func ensureCategoryPath(ctx context.Context, tx pgx.Tx, uid int64, path []string, kind string) error {
	var parent *int64
	for _, name := range path {
		var id int64
		err := tx.QueryRow(ctx, `SELECT id FROM categories WHERE user_id = $1 AND name = $2`, uid, name).Scan(&id)
		if errors.Is(err, pgx.ErrNoRows) {
			err = tx.QueryRow(ctx, `
				INSERT INTO categories (user_id, name, parent_id, kind) VALUES ($1, $2, $3, $4) RETURNING id`,
				uid, name, parent, kind).Scan(&id)
		}
		if err != nil {
			return err
		}
		parent = &id
	}
	return nil
}
