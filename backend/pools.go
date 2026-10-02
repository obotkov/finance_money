package main

import (
	"context"
	"errors"
	"math"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Пулы ликвидности в крипто-портфеле. Позиция — пара монет A/B с ценовым
// интервалом (цена — сколько B за одну A). Каждое внесение по своей цене даёт
// ликвидность L; сколько монет сейчас в позиции, считается из суммы L и цены:
// ниже интервала всё лежит в A, выше — в B, внутри — смесь (формулы Uniswap v3).
// Страница считает так же — poolAmounts/poolLiquidity в index.html.
//
// Монеты ходят между позицией и строками монет того же портфеля: внесение
// берёт их из портфеля (а чего там нет — считается новыми деньгами по курсу дня),
// вывод и комиссии возвращают туда. «Вложено» переезжает вместе с монетами
// пропорционально, поэтому прибыль портфеля от внесения или вывода не меняется.

type Pool struct {
	ID     int64       `json:"id"`
	CoinA  string      `json:"coinA"`
	CoinB  string      `json:"coinB"`
	Min    float64     `json:"min"`
	Max    float64     `json:"max"`
	Place  string      `json:"place"`
	Events []PoolEvent `json:"events"`
}

type PoolEvent struct {
	ID        int64   `json:"id"`
	Kind      string  `json:"kind"` // deposit, withdraw, fees
	Date      string  `json:"date"`
	Price     float64 `json:"price"`
	AmountA   float64 `json:"amountA"`
	AmountB   float64 `json:"amountB"`
	Liquidity float64 `json:"liquidity"`
	Invested  float64 `json:"invested"`
	// как событие изменило монеты портфеля — график стоимости откатывает это для дней до него
	AssetA float64 `json:"assetA"`
	AssetB float64 `json:"assetB"`
}

// poolAmounts — сколько монет A и B даёт ликвидность L при цене p в интервале [lo, hi].
func poolAmounts(L, p, lo, hi float64) (a, b float64) {
	sp := math.Sqrt(math.Min(math.Max(p, lo), hi))
	return L * (1/sp - 1/math.Sqrt(hi)), L * (sp - math.Sqrt(lo))
}

// poolLiquidity — ликвидность внесения a и b при цене p. Как в протоколе,
// берётся меньшая из двух: лишнее одной из монет в позицию не попадает. Внутри
// интервала нужны обе монеты, ниже — только A, выше — только B.
func poolLiquidity(a, b, p, lo, hi float64) float64 {
	sp, sl, sh := math.Sqrt(math.Min(math.Max(p, lo), hi)), math.Sqrt(lo), math.Sqrt(hi)
	L := math.Inf(1)
	if sp < sh {
		if !(a > 0) {
			return 0
		}
		L = math.Min(L, a/(1/sp-1/sh))
	}
	if sp > sl {
		if !(b > 0) {
			return 0
		}
		L = math.Min(L, b/(sp-sl))
	}
	if math.IsInf(L, 1) {
		return 0
	}
	return L
}

func (s *Store) poolsOf(ctx context.Context, uid int64) (map[int64][]Pool, error) {
	rows, _ := s.db.Query(ctx, `
		SELECT portfolio_id, id, coin_a, coin_b, price_min, price_max, place
		FROM lp_positions WHERE user_id = $1 ORDER BY id`, uid)
	type row struct {
		portfolio int64
		pool      Pool
	}
	list, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (row, error) {
		var x row
		x.pool.Events = []PoolEvent{}
		err := r.Scan(&x.portfolio, &x.pool.ID, &x.pool.CoinA, &x.pool.CoinB, &x.pool.Min, &x.pool.Max, &x.pool.Place)
		return x, err
	})
	if err != nil {
		return nil, err
	}
	at := map[int64]*Pool{}
	for i := range list {
		at[list[i].pool.ID] = &list[i].pool
	}
	rows, _ = s.db.Query(ctx, `
		SELECT position_id, id, kind, to_char(day, 'YYYY-MM-DD'), price, amount_a, amount_b, liquidity, invested, asset_a, asset_b
		FROM lp_events WHERE user_id = $1 ORDER BY day, id`, uid)
	defer rows.Close()
	for rows.Next() {
		var pos int64
		var e PoolEvent
		if err := rows.Scan(&pos, &e.ID, &e.Kind, &e.Date, &e.Price, &e.AmountA, &e.AmountB, &e.Liquidity, &e.Invested, &e.AssetA, &e.AssetB); err != nil {
			return nil, err
		}
		if p := at[pos]; p != nil {
			p.Events = append(p.Events, e)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := map[int64][]Pool{}
	for _, x := range list {
		out[x.portfolio] = append(out[x.portfolio], x.pool)
	}
	return out, nil
}

type PoolInput struct {
	Portfolio int64   `json:"portfolio"`
	CoinA     string  `json:"coinA"`
	CoinB     string  `json:"coinB"`
	Min       float64 `json:"min"`
	Max       float64 `json:"max"`
	Place     string  `json:"place"`
	// первое внесение
	Date    string  `json:"date"`
	Price   float64 `json:"price"`
	AmountA float64 `json:"amountA"`
	AmountB float64 `json:"amountB"`
}

type PoolEventInput struct {
	Kind    string  `json:"kind"`
	Date    string  `json:"date"`
	Price   float64 `json:"price"`
	AmountA float64 `json:"amountA"`
	AmountB float64 `json:"amountB"`
	// вывод: какая доля ликвидности выводится, в процентах
	Percent float64 `json:"percent"`
}

type poolRow struct {
	id, portfolio int64
	coinA, coinB  string
	lo, hi        float64
}

func parseDay(s string) (time.Time, error) {
	d, err := time.Parse(time.DateOnly, s)
	if err != nil {
		return d, badRequest("Дата должна быть в формате ГГГГ-ММ-ДД")
	}
	if d.After(moscowToday()) {
		return d, badRequest("Дата не может быть в будущем")
	}
	return d, nil
}

func (s *Store) CreatePool(ctx context.Context, uid int64, in PoolInput) error {
	in.CoinA, in.CoinB = strings.ToUpper(strings.TrimSpace(in.CoinA)), strings.ToUpper(strings.TrimSpace(in.CoinB))
	in.Place = strings.TrimSpace(in.Place)
	switch {
	case coinID(in.CoinA) == "" || coinID(in.CoinB) == "":
		return badRequest("Выберите обе монеты пары")
	case in.CoinA == in.CoinB:
		return badRequest("Монеты пары должны быть разными")
	case !(in.Min > 0) || !(in.Max > in.Min):
		return badRequest("Интервал: нижняя цена больше нуля и меньше верхней")
	case !(in.Price > 0):
		return badRequest("Укажите цену " + in.CoinA + " в " + in.CoinB)
	}
	day, err := parseDay(in.Date)
	if err != nil {
		return err
	}
	return pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		var exists bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM crypto_portfolios WHERE id = $1 AND user_id = $2)`,
			in.Portfolio, uid).Scan(&exists); err != nil {
			return err
		}
		if !exists {
			return notFound("Портфель не найден")
		}
		p := poolRow{portfolio: in.Portfolio, coinA: in.CoinA, coinB: in.CoinB, lo: in.Min, hi: in.Max}
		if err := tx.QueryRow(ctx, `
			INSERT INTO lp_positions (user_id, portfolio_id, coin_a, coin_b, price_min, price_max, place)
			VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING id`,
			uid, in.Portfolio, in.CoinA, in.CoinB, in.Min, in.Max, in.Place).Scan(&p.id); err != nil {
			return err
		}
		return poolDeposit(ctx, tx, uid, p, day, in.Price, in.AmountA, in.AmountB)
	})
}

func (s *Store) UpdatePool(ctx context.Context, uid, id int64, in struct {
	Place string `json:"place"`
}) error {
	tag, err := s.db.Exec(ctx, `UPDATE lp_positions SET place = $3 WHERE id = $1 AND user_id = $2`, id, uid, strings.TrimSpace(in.Place))
	if err == nil && tag.RowsAffected() == 0 {
		return notFound("Позиция не найдена")
	}
	return err
}

func (s *Store) AddPoolEvent(ctx context.Context, uid, id int64, in PoolEventInput) error {
	day, err := parseDay(in.Date)
	if err != nil {
		return err
	}
	if !(in.Price > 0) {
		return badRequest("Укажите цену")
	}
	if in.AmountA < 0 || in.AmountB < 0 {
		return badRequest("Количество монет не может быть отрицательным")
	}
	return pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		p, err := lockPool(ctx, tx, uid, id)
		if err != nil {
			return err
		}
		// события идут по порядку: ликвидность и вложенное — суммы по времени
		var last *time.Time
		if err := tx.QueryRow(ctx, `SELECT max(day) FROM lp_events WHERE position_id = $1`, id).Scan(&last); err != nil {
			return err
		}
		if last != nil && day.Before(*last) {
			return badRequest("Дата не раньше последнего события позиции — " + last.Format("02.01.2006"))
		}
		switch in.Kind {
		case "deposit":
			return poolDeposit(ctx, tx, uid, p, day, in.Price, in.AmountA, in.AmountB)
		case "withdraw":
			return poolWithdraw(ctx, tx, uid, p, day, in.Price, in.Percent, in.AmountA, in.AmountB)
		case "fees":
			if !(in.AmountA > 0) && !(in.AmountB > 0) {
				return badRequest("Укажите, сколько монет пришло комиссиями")
			}
			if err := moveAsset(ctx, tx, uid, p.portfolio, p.coinA, in.AmountA, 0); err != nil {
				return err
			}
			if err := moveAsset(ctx, tx, uid, p.portfolio, p.coinB, in.AmountB, 0); err != nil {
				return err
			}
			_, err := tx.Exec(ctx, `
				INSERT INTO lp_events (user_id, position_id, kind, day, price, amount_a, amount_b, asset_a, asset_b)
				VALUES ($1, $2, 'fees', $3, $4, $5, $6, $5, $6)`, uid, p.id, day, in.Price, in.AmountA, in.AmountB)
			return err
		}
		return badRequest("Неизвестное действие с позицией")
	})
}

func lockPool(ctx context.Context, tx pgx.Tx, uid, id int64) (poolRow, error) {
	p := poolRow{id: id}
	err := tx.QueryRow(ctx, `
		SELECT portfolio_id, coin_a, coin_b, price_min, price_max FROM lp_positions
		WHERE id = $1 AND user_id = $2 FOR UPDATE`, id, uid).Scan(&p.portfolio, &p.coinA, &p.coinB, &p.lo, &p.hi)
	if errors.Is(err, pgx.ErrNoRows) {
		return p, notFound("Позиция не найдена")
	}
	return p, err
}

// poolDeposit вносит в позицию a монет A и b монет B по цене price.
func poolDeposit(ctx context.Context, tx pgx.Tx, uid int64, p poolRow, day time.Time, price, a, b float64) error {
	L := poolLiquidity(a, b, price, p.lo, p.hi)
	if !(L > 0) {
		switch {
		case price <= p.lo:
			return badRequest("Цена ниже интервала — вносится только " + p.coinA)
		case price >= p.hi:
			return badRequest("Цена выше интервала — вносится только " + p.coinB)
		}
		return badRequest("Цена внутри интервала — внесите обе монеты, " + p.coinA + " и " + p.coinB)
	}
	// сколько монет на самом деле ушло в позицию: лишнее одной из них остаётся в портфеле
	ua, ub := poolAmounts(L, price, p.lo, p.hi)
	ua, ub = math.Min(ua, a), math.Min(ub, b)
	var invested float64
	moved := [2]struct{ take, inv float64 }{}
	for i, c := range []struct {
		coin string
		q    float64
	}{{p.coinA, ua}, {p.coinB, ub}} {
		if !(c.q > 0) {
			continue
		}
		var have, haveInv float64
		err := tx.QueryRow(ctx, `
			SELECT amount, invested FROM crypto_assets WHERE user_id = $1 AND portfolio_id = $2 AND coin = $3 FOR UPDATE`,
			uid, p.portfolio, c.coin).Scan(&have, &haveInv)
		if err != nil && !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		take := math.Min(c.q, have)
		if take > 0 {
			moved[i].take, moved[i].inv = take, haveInv*take/have
			if err := moveAsset(ctx, tx, uid, p.portfolio, c.coin, -take, -moved[i].inv); err != nil {
				return err
			}
		}
		invested += moved[i].inv
		// чего в портфеле не было — новые деньги, вложены по курсу дня внесения
		if ext := c.q - take; ext > 1e-12 {
			usdt, err := usdtOn(ctx, tx, c.coin, day)
			if err != nil {
				return err
			}
			invested += ext * usdt
		}
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO lp_events (user_id, position_id, kind, day, price, amount_a, amount_b, liquidity, invested,
		                       asset_a, asset_b, asset_inv_a, asset_inv_b)
		VALUES ($1, $2, 'deposit', $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`,
		uid, p.id, day, price, ua, ub, L, invested, -moved[0].take, -moved[1].take, -moved[0].inv, -moved[1].inv)
	return err
}

// poolWithdraw выводит percent процентов ликвидности; a и b — сколько монет
// пришло на самом деле (страница подставляет расчётное). Вложенное уходит
// в монеты портфеля той же долей, поделённое по их стоимости.
func poolWithdraw(ctx context.Context, tx pgx.Tx, uid int64, p poolRow, day time.Time, price, percent, a, b float64) error {
	if !(percent > 0) || percent > 100 {
		return badRequest("Сколько вывести — от 0 до 100 %")
	}
	var L, inv float64
	if err := tx.QueryRow(ctx, `
		SELECT COALESCE(sum(liquidity), 0), COALESCE(sum(invested), 0) FROM lp_events WHERE position_id = $1`, p.id).Scan(&L, &inv); err != nil {
		return err
	}
	if !(L > 1e-12) {
		return badRequest("Позиция уже закрыта — выводить нечего")
	}
	f := percent / 100
	lw, iw := L*f, inv*f
	if percent == 100 {
		lw, iw = L, inv // ровно всё, без хвостов от округления
	}
	if !(a > 0) && !(b > 0) {
		a, b = poolAmounts(lw, price, p.lo, p.hi)
	}
	invA := iw
	if va, vb := a*price, b; va+vb > 0 {
		invA = iw * va / (va + vb)
	}
	invB := iw - invA
	if err := moveAsset(ctx, tx, uid, p.portfolio, p.coinA, a, invA); err != nil {
		return err
	}
	if err := moveAsset(ctx, tx, uid, p.portfolio, p.coinB, b, invB); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO lp_events (user_id, position_id, kind, day, price, amount_a, amount_b, liquidity, invested,
		                       asset_a, asset_b, asset_inv_a, asset_inv_b)
		VALUES ($1, $2, 'withdraw', $3, $4, $5, $6, $7, $8, $5, $6, $9, $10)`,
		uid, p.id, day, price, a, b, -lw, -iw, invA, invB)
	return err
}

// moveAsset меняет строку монеты в портфеле на da монет и dInv вложенного:
// заводит строку, если её нет, и убирает опустевшую.
func moveAsset(ctx context.Context, tx pgx.Tx, uid, portfolio int64, coin string, da, dInv float64) error {
	if da == 0 && dInv == 0 {
		return nil
	}
	var id int64
	var amount, inv float64
	err := tx.QueryRow(ctx, `
		SELECT id, amount, invested FROM crypto_assets WHERE user_id = $1 AND portfolio_id = $2 AND coin = $3 FOR UPDATE`,
		uid, portfolio, coin).Scan(&id, &amount, &inv)
	if errors.Is(err, pgx.ErrNoRows) {
		if da < -1e-12 {
			return badRequest("В портфеле нет " + coin + ", чтобы списать их")
		}
		_, err = tx.Exec(ctx, `
			INSERT INTO crypto_assets (user_id, portfolio_id, coin, amount, invested) VALUES ($1, $2, $3, $4, $5)`,
			uid, portfolio, coin, math.Max(da, 0), math.Max(dInv, 0))
		return err
	}
	if err != nil {
		return err
	}
	amount, inv = amount+da, inv+dInv
	if amount < -1e-9 {
		return badRequest("В портфеле уже меньше " + coin + ", чем нужно вернуть — сначала поправьте монету")
	}
	amount, inv = math.Max(amount, 0), math.Max(inv, 0)
	if amount < 1e-12 && inv < 1e-6 {
		_, err = tx.Exec(ctx, `DELETE FROM crypto_assets WHERE id = $1`, id)
		return err
	}
	_, err = tx.Exec(ctx, `UPDATE crypto_assets SET amount = $2, invested = $3 WHERE id = $1`, id, amount, inv)
	return err
}

// usdtOn — цена монеты в USDT в день day: по истории курсов, а без неё — по текущему.
func usdtOn(ctx context.Context, tx pgx.Tx, coin string, day time.Time) (float64, error) {
	if coin == "USDT" {
		return 1, nil
	}
	rub := func(code string) (float64, error) {
		var v float64
		err := tx.QueryRow(ctx, `
			SELECT COALESCE(
				(SELECT rub FROM rate_history WHERE code = $1 AND day <= $2 ORDER BY day DESC LIMIT 1),
				(SELECT rub FROM rates WHERE code = $1), 0)`, code, day).Scan(&v)
		return v, err
	}
	c, err := rub(coin)
	if err != nil {
		return 0, err
	}
	u, err := rub("USDT")
	if err != nil || !(u > 0) {
		return 0, err
	}
	return c / u, nil
}

// DeletePoolEvent удаляет последнее событие позиции и возвращает монеты
// портфеля как было. Удалено единственное внесение — уходит и позиция.
func (s *Store) DeletePoolEvent(ctx context.Context, uid, id, eventID int64) error {
	return pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		p, err := lockPool(ctx, tx, uid, id)
		if err != nil {
			return err
		}
		var last int64
		if err := tx.QueryRow(ctx, `
			SELECT id FROM lp_events WHERE position_id = $1 ORDER BY day DESC, id DESC LIMIT 1`, id).Scan(&last); err != nil {
			return err
		}
		if last != eventID {
			return badRequest("Удалить можно только последнее событие позиции")
		}
		if err := revertPoolEvent(ctx, tx, uid, p, eventID); err != nil {
			return err
		}
		var left int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM lp_events WHERE position_id = $1`, id).Scan(&left); err != nil {
			return err
		}
		if left == 0 {
			_, err = tx.Exec(ctx, `DELETE FROM lp_positions WHERE id = $1`, id)
		}
		return err
	})
}

// DeletePool удаляет позицию, отменяя её события с конца: монеты портфеля
// становятся такими, какими были до неё.
func (s *Store) DeletePool(ctx context.Context, uid, id int64) error {
	return pgx.BeginFunc(ctx, s.db, func(tx pgx.Tx) error {
		p, err := lockPool(ctx, tx, uid, id)
		if err != nil {
			return err
		}
		rows, _ := tx.Query(ctx, `SELECT id FROM lp_events WHERE position_id = $1 ORDER BY day DESC, id DESC`, id)
		ids, err := pgx.CollectRows(rows, pgx.RowTo[int64])
		if err != nil {
			return err
		}
		for _, e := range ids {
			if err := revertPoolEvent(ctx, tx, uid, p, e); err != nil {
				return err
			}
		}
		_, err = tx.Exec(ctx, `DELETE FROM lp_positions WHERE id = $1`, id)
		return err
	})
}

func revertPoolEvent(ctx context.Context, tx pgx.Tx, uid int64, p poolRow, eventID int64) error {
	var a, b, ia, ib float64
	if err := tx.QueryRow(ctx, `
		DELETE FROM lp_events WHERE id = $1 AND position_id = $2
		RETURNING asset_a, asset_b, asset_inv_a, asset_inv_b`, eventID, p.id).Scan(&a, &b, &ia, &ib); err != nil {
		return err
	}
	if err := moveAsset(ctx, tx, uid, p.portfolio, p.coinA, -a, -ia); err != nil {
		return err
	}
	return moveAsset(ctx, tx, uid, p.portfolio, p.coinB, -b, -ib)
}
