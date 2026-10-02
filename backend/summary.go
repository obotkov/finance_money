package main

import (
	"context"
	"fmt"
	"html"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Сводка по финансам для Telegram — текстом в HTML-разметке Telegram. Суммы
// в рублях по текущим курсам, как на странице: у крипто-счёта — остаток плюс
// купленные на нём монеты и позиции в пулах. Доходы и расходы — без переводов
// и сделок.

var monthsGen = []string{"января", "февраля", "марта", "апреля", "мая", "июня", "июля", "августа", "сентября", "октября", "ноября", "декабря"}

// summaryRange — неделя сводки: по расписанию (в понедельник) — прошлая неделя
// с понедельника по воскресенье, по кнопке — последние семь дней.
func summaryRange(today time.Time, weekly bool) (from, to time.Time) {
	if weekly {
		monday := today.AddDate(0, 0, -((int(today.Weekday()) + 6) % 7))
		return monday.AddDate(0, 0, -7), monday.AddDate(0, 0, -1)
	}
	return today.AddDate(0, 0, -6), today
}

// rangeLabel — «22–28 сентября» или «29 сентября — 5 октября».
func rangeLabel(from, to time.Time) string {
	if from.Month() == to.Month() {
		return fmt.Sprintf("%d–%d %s", from.Day(), to.Day(), monthsGen[to.Month()-1])
	}
	return fmt.Sprintf("%d %s — %d %s", from.Day(), monthsGen[from.Month()-1], to.Day(), monthsGen[to.Month()-1])
}

type flowRow struct {
	Type, Cur string
	Amount    float64
	CatID     *int64
	CatName   string
	Day       time.Time
}

// FinanceSummary собирает сводку пользователя на сегодня (по Москве).
func (s *Store) FinanceSummary(ctx context.Context, uid int64, weekly bool) (string, error) {
	today := moscowToday()
	rates := map[string]float64{"RUB": 1}
	rows, _ := s.db.Query(ctx, `SELECT code, rub FROM rates`)
	list, err := pgx.CollectRows(rows, pgx.RowToStructByPos[struct {
		Code string
		Rub  float64
	}])
	if err != nil {
		return "", err
	}
	for _, r := range list {
		rates[r.Code] = r.Rub
	}
	rub := func(v float64, cur string) float64 { return v * rates[cur] }
	poolRub := func(p Pool) float64 {
		var L float64
		for _, e := range p.Events {
			L += e.Liquidity
		}
		if !(L > 1e-12) || !(rates[p.CoinB] > 0) {
			return 0
		}
		a, b := poolAmounts(L, rates[p.CoinA]/rates[p.CoinB], p.Min, p.Max)
		return rub(a, p.CoinA) + rub(b, p.CoinB)
	}
	byPortfolio, byAccount, err := s.poolsOf(ctx, uid)
	if err != nil {
		return "", err
	}

	// счета: остаток, у крипто-счёта — плюс монеты и пулы
	rows, _ = s.db.Query(ctx, `SELECT id, kind, currency, balance FROM accounts WHERE user_id = $1`, uid)
	accs, err := pgx.CollectRows(rows, pgx.RowToStructByPos[struct {
		ID        int64
		Kind, Cur string
		Balance   float64
	}])
	if err != nil {
		return "", err
	}
	var total, free float64
	for _, a := range accs {
		v := rub(a.Balance, a.Cur)
		if a.Kind == cryptoKind {
			held := map[string]float64{}
			rows, _ := s.db.Query(ctx, `
				SELECT coin, COALESCE(sum(CASE type WHEN 'buy' THEN received ELSE -received END), 0) FROM transactions
				WHERE user_id = $1 AND account_id = $2 AND type IN ('buy', 'sell') AND close_price IS NULL GROUP BY coin`, uid, a.ID)
			coins, err := pgx.CollectRows(rows, pgx.RowToStructByPos[struct {
				Coin string
				Qty  float64
			}])
			if err != nil {
				return "", err
			}
			for _, c := range coins {
				held[c.Coin] += c.Qty
			}
			for _, p := range byAccount[a.ID] {
				for _, e := range p.Events {
					if p.CoinA != a.Cur {
						held[p.CoinA] += e.AssetA
					}
					if p.CoinB != a.Cur {
						held[p.CoinB] += e.AssetB
					}
				}
				v += poolRub(p)
			}
			for coin, q := range held {
				if q > 1e-12 {
					v += rub(q, coin)
				}
			}
		}
		total += v
		if a.Kind == "Карта" || a.Kind == "Наличные" {
			free += v
		}
	}

	// крипто-портфели — отдельно, в «Всего» они не входят, как и на странице
	var ports float64
	rows, _ = s.db.Query(ctx, `SELECT coin, amount FROM crypto_assets WHERE user_id = $1`, uid)
	assets, err := pgx.CollectRows(rows, pgx.RowToStructByPos[struct {
		Coin   string
		Amount float64
	}])
	if err != nil {
		return "", err
	}
	for _, a := range assets {
		ports += rub(a.Amount, a.Coin)
	}
	for _, list := range byPortfolio {
		for _, p := range list {
			ports += poolRub(p)
		}
	}

	// доходы и расходы с начала прошлой недели (её хватает и на месяц, и на неделю)
	from, to := summaryRange(today, weekly)
	monthStart := time.Date(today.Year(), today.Month(), 1, 0, 0, 0, 0, time.UTC)
	since := from
	if monthStart.Before(since) {
		since = monthStart
	}
	rows, _ = s.db.Query(ctx, `
		SELECT t.type, a.currency, t.amount, t.category_id, t.category_name, t.date
		FROM transactions t JOIN accounts a ON a.id = t.account_id
		WHERE t.user_id = $1 AND t.type IN ('income', 'expense') AND t.date >= $2 AND t.date <= $3`, uid, since, today)
	flows, err := pgx.CollectRows(rows, pgx.RowToStructByPos[flowRow])
	if err != nil {
		return "", err
	}
	rows, _ = s.db.Query(ctx, `SELECT id, name, parent_id FROM categories WHERE user_id = $1`, uid)
	cats, err := pgx.CollectRows(rows, pgx.RowToStructByPos[struct {
		ID     int64
		Name   string
		Parent *int64
	}])
	if err != nil {
		return "", err
	}
	parent, names := map[int64]*int64{}, map[int64]string{}
	for _, c := range cats {
		parent[c.ID], names[c.ID] = c.Parent, c.Name
	}
	root := func(f flowRow) string {
		if f.CatID == nil {
			return f.CatName
		}
		id := *f.CatID
		for i := 0; i < 8 && parent[id] != nil; i++ {
			id = *parent[id]
		}
		if n := names[id]; n != "" {
			return n
		}
		return f.CatName
	}
	var wIn, wOut, mIn, mOut float64
	byCat := map[string]float64{}
	for _, f := range flows {
		v := rub(f.Amount, f.Cur)
		inWeek := !f.Day.Before(from) && !f.Day.After(to)
		inMonth := !f.Day.Before(monthStart)
		if f.Type == "income" {
			if inWeek {
				wIn += v
			}
			if inMonth {
				mIn += v
			}
			continue
		}
		if inWeek {
			wOut += v
			byCat[root(f)] += v
		}
		if inMonth {
			mOut += v
		}
	}

	var b strings.Builder
	esc := html.EscapeString
	fmt.Fprintf(&b, "<b>Penny · сводка за %s</b>\n\n", rangeLabel(from, to))
	fmt.Fprintf(&b, "💰 <b>Всего на счетах:</b> %s\n", rub2(total))
	fmt.Fprintf(&b, "Свободные деньги (карты и наличные): %s\n", rub2(free))
	if ports > 0.5 {
		usdt := ""
		if rates["USDT"] > 0 {
			usdt = fmt.Sprintf(" (%s USDT)", groupInt(ports/rates["USDT"]))
		}
		fmt.Fprintf(&b, "Крипто-портфели: %s%s\n", rub2(ports), usdt)
	}

	fmt.Fprintf(&b, "\n<b>За неделю</b>\nДоходы: %s\nРасходы: %s\nИтог: %s\n", signedRub(wIn), signedRub(-wOut), signedRub(wIn-wOut))
	if len(byCat) > 0 {
		type kv struct {
			k string
			v float64
		}
		var top []kv
		for k, v := range byCat {
			top = append(top, kv{k, v})
		}
		sort.Slice(top, func(i, j int) bool { return top[i].v > top[j].v })
		if len(top) > 5 {
			top = top[:5]
		}
		parts := make([]string, len(top))
		for i, t := range top {
			parts[i] = esc(t.k) + " " + rub2(t.v)
		}
		fmt.Fprintf(&b, "Больше всего: %s\n", strings.Join(parts, " · "))
	}
	fmt.Fprintf(&b, "\n<b>С начала месяца</b>\nДоходы: %s · Расходы: %s\n", signedRub(mIn), signedRub(-mOut))

	// бюджеты у порога и превышенные
	usage, err := s.budgetUsage(ctx, uid, moscowMonth())
	if err != nil {
		return "", err
	}
	alert, err := s.budgetAlert(ctx, uid)
	if err != nil {
		return "", err
	}
	if len(usage) > 0 {
		var lines []string
		for _, u := range usage {
			pct := int(math.Floor(u.Spent / u.Limit * 100))
			switch budgetLevel(u.Spent, u.Limit, alert.Pct) {
			case "over":
				lines = append(lines, fmt.Sprintf("🔴 %s: %s из %s — на %s больше", esc(u.Category), rub2(u.Spent), rub2(u.Limit), rub2(u.Spent-u.Limit)))
			case "warn":
				lines = append(lines, fmt.Sprintf("🟡 %s: %s из %s (%d %%)", esc(u.Category), rub2(u.Spent), rub2(u.Limit), pct))
			}
		}
		b.WriteString("\n<b>Бюджеты</b>\n")
		if len(lines) == 0 {
			fmt.Fprintf(&b, "Все в норме — ни один не дошёл до %d %%\n", alert.Pct)
		} else {
			b.WriteString(strings.Join(lines, "\n") + "\n")
		}
	}

	// повторяющиеся операции на ближайшую неделю
	rows, _ = s.db.Query(ctx, `
		SELECT r.type, a.currency, r.amount, r.category, r.title, r.next_date
		FROM recurring r JOIN accounts a ON a.id = r.account_id
		WHERE r.user_id = $1 AND r.next_date <= $2 ORDER BY r.next_date, r.id`, uid, today.AddDate(0, 0, 7))
	next, err := pgx.CollectRows(rows, pgx.RowToStructByPos[struct {
		Type, Cur       string
		Amount          float64
		Category, Title string
		Day             time.Time
	}])
	if err != nil {
		return "", err
	}
	if len(next) > 0 {
		b.WriteString("\n<b>Ближайшие 7 дней</b>\n")
		for _, r := range next {
			name := r.Title
			if name == "" {
				name = r.Category
			}
			v := rub(r.Amount, r.Cur)
			if r.Type == "expense" {
				v = -v
			}
			fmt.Fprintf(&b, "%d %s — %s %s\n", r.Day.Day(), monthsGen[r.Day.Month()-1], esc(name), signedRub(v))
		}
	}
	return strings.TrimRight(b.String(), "\n"), nil
}

// rub2 — «12 345 ₽» (как rub из push.go), signedRub — со знаком плюс у положительных.
func rub2(v float64) string { return rub(v) }

func signedRub(v float64) string {
	if math.Round(v) > 0 {
		return "+" + rub(v)
	}
	return rub(v)
}

// groupInt — целое с пробелами между разрядами.
func groupInt(v float64) string { return strings.TrimSuffix(rub(v), " ₽") }
