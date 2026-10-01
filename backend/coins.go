package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
)

// Свои монеты. Встроенные лежат в coinIDs; найденные в CoinGecko и добавленные
// пользователями — в таблице coins и в памяти (extraCoins), чтобы курсы,
// проверка валюты счёта и сделок видели их наравне со встроенными.

const (
	coingeckoSearchURL  = "https://api.coingecko.com/api/v3/search?query=%s"
	coingeckoMarketsURL = "https://api.coingecko.com/api/v3/coins/markets?vs_currency=rub&ids=%s"
	// цены найденных монет — чтобы по цене было видно, та ли это монета
	coingeckoSearchPriceURL = "https://api.coingecko.com/api/v3/simple/price?vs_currencies=usd,rub&ids=%s"
	// сколько своих монет может быть на сервере: каждая — ещё один курс в каждом обновлении
	maxExtraCoins = 300
	// поиск CoinGecko без ключа — несколько запросов в минуту, поэтому ответы запоминаются
	searchCacheTTL = 10 * time.Minute
)

var extraCoins = struct {
	sync.RWMutex
	ids map[string]string // тикер → id в CoinGecko
}{ids: map[string]string{}}

// coinID — id монеты в CoinGecko по тикеру, "" — монета неизвестна.
func coinID(code string) string {
	if id := coinIDs[code]; id != "" {
		return id
	}
	extraCoins.RLock()
	defer extraCoins.RUnlock()
	return extraCoins.ids[code]
}

// allCoinIDs — все отслеживаемые монеты: встроенные и свои.
func allCoinIDs() map[string]string {
	extraCoins.RLock()
	defer extraCoins.RUnlock()
	out := make(map[string]string, len(coinIDs)+len(extraCoins.ids))
	for code, id := range extraCoins.ids {
		out[code] = id
	}
	for code, id := range coinIDs {
		out[code] = id
	}
	return out
}

func registerCoin(code, id string) {
	extraCoins.Lock()
	extraCoins.ids[code] = id
	extraCoins.Unlock()
}

// isCurrency — можно ли держать счёт в этой валюте: рубли, доллары, евро и любая монета.
func isCurrency(code string) bool {
	return code == "RUB" || code == "USD" || code == "EUR" || coinID(code) != ""
}

// LoadCoins поднимает свои монеты из базы при запуске.
func (s *Store) LoadCoins(ctx context.Context) error {
	rows, _ := s.db.Query(ctx, `SELECT code, gecko_id FROM coins`)
	list, err := pgx.CollectRows(rows, pgx.RowToStructByPos[struct{ Code, ID string }])
	if err != nil {
		return err
	}
	for _, c := range list {
		registerCoin(c.Code, c.ID)
	}
	return nil
}

// Coin — своя монета в списке пользователя.
type Coin struct {
	Code  string `json:"code"`
	Name  string `json:"name"`
	Image string `json:"image"`
}

func (s *Store) userCoins(ctx context.Context, uid int64) ([]Coin, error) {
	rows, _ := s.db.Query(ctx, `
		SELECT c.code, c.name, c.image FROM user_coins u JOIN coins c ON c.code = u.code
		WHERE u.user_id = $1 ORDER BY u.created_at, c.code`, uid)
	return pgx.CollectRows(rows, pgx.RowToStructByPos[Coin])
}

// FoundCoin — монета из поиска; Status: builtin — встроенная, tracked — уже в
// списке пользователя, taken — тикер занят другой монетой, "" — можно добавить.
type FoundCoin struct {
	ID    string `json:"id"`
	Code  string `json:"code"`
	Name  string `json:"name"`
	Thumb string `json:"thumb"`
	Rank  int    `json:"rank"`
	// цена сейчас; 0 — CoinGecko её не знает (или не ответил)
	Usd    float64 `json:"usd"`
	Rub    float64 `json:"rub"`
	Status string  `json:"status"`
	// для taken — какая монета занимает тикер
	TakenBy string `json:"takenBy,omitempty"`
}

var tickerRe = regexp.MustCompile(`^[A-Z0-9]{1,12}$`)

// reservedCode — тикеры, которые уже значат валюту или металл в таблице курсов.
func reservedCode(code string) bool {
	if code == "RUB" || slices.Contains(cbrCodes, code) {
		return true
	}
	for _, m := range metals {
		if m.code == code {
			return true
		}
	}
	return false
}

type searchEntry struct {
	at    time.Time
	coins []FoundCoin
}

var searchCache = struct {
	sync.Mutex
	m map[string]searchEntry
}{m: map[string]searchEntry{}}

// SearchCoins ищет монеты в CoinGecko по названию или тикеру — первые 12,
// по капитализации, как их отдаёт CoinGecko.
func (u *RateUpdater) SearchCoins(ctx context.Context, q string) ([]FoundCoin, error) {
	q = strings.ToLower(strings.TrimSpace(q))
	if len([]rune(q)) < 2 {
		return []FoundCoin{}, nil
	}
	searchCache.Lock()
	e, ok := searchCache.m[q]
	searchCache.Unlock()
	if !ok || time.Since(e.at) > searchCacheTTL {
		body, err := u.get(ctx, fmt.Sprintf(coingeckoSearchURL, url.QueryEscape(q)))
		if err != nil {
			return nil, err
		}
		coins, err := parseCoinGeckoSearch(body)
		if err != nil {
			return nil, err
		}
		u.priceFound(ctx, coins)
		e = searchEntry{at: time.Now(), coins: coins}
		searchCache.Lock()
		if len(searchCache.m) > 500 {
			clear(searchCache.m)
		}
		searchCache.m[q] = e
		searchCache.Unlock()
	}
	return slices.Clone(e.coins), nil
}

// priceFound дописывает найденным монетам цену. Без цены поиск всё равно
// полезен, поэтому ошибка только пишется в лог.
func (u *RateUpdater) priceFound(ctx context.Context, coins []FoundCoin) {
	if len(coins) == 0 {
		return
	}
	ids := make([]string, len(coins))
	for i, c := range coins {
		ids[i] = url.QueryEscape(c.ID)
	}
	body, err := u.get(ctx, fmt.Sprintf(coingeckoSearchPriceURL, strings.Join(ids, ",")))
	var prices map[string]map[string]float64
	if err == nil {
		err = json.Unmarshal(body, &prices)
	}
	if err != nil {
		u.log.Warn("coin search prices", "err", err)
		return
	}
	for i := range coins {
		coins[i].Usd, coins[i].Rub = prices[coins[i].ID]["usd"], prices[coins[i].ID]["rub"]
	}
}

func parseCoinGeckoSearch(body []byte) ([]FoundCoin, error) {
	var resp struct {
		Coins []struct {
			ID     string `json:"id"`
			Name   string `json:"name"`
			Symbol string `json:"symbol"`
			Rank   int    `json:"market_cap_rank"`
			Thumb  string `json:"thumb"`
		} `json:"coins"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("parse CoinGecko search: %w", err)
	}
	out := []FoundCoin{}
	for _, c := range resp.Coins {
		code := strings.ToUpper(strings.TrimSpace(c.Symbol))
		if c.ID == "" || !tickerRe.MatchString(code) {
			continue
		}
		out = append(out, FoundCoin{ID: c.ID, Code: code, Name: c.Name, Thumb: c.Thumb, Rank: c.Rank})
		if len(out) == 12 {
			break
		}
	}
	return out, nil
}

// MarkFound проставляет найденным монетам, можно ли их добавить этому пользователю.
func (s *Store) MarkFound(ctx context.Context, uid int64, found []FoundCoin) error {
	rows, _ := s.db.Query(ctx, `SELECT code FROM user_coins WHERE user_id = $1`, uid)
	mine, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return err
	}
	names := map[string]string{} // тикер своей монеты → название, для «занят»
	rows, _ = s.db.Query(ctx, `SELECT code, name FROM coins`)
	all, err := pgx.CollectRows(rows, pgx.RowToStructByPos[struct{ Code, Name string }])
	if err != nil {
		return err
	}
	for _, c := range all {
		names[c.Code] = c.Name
	}
	for i := range found {
		f := &found[i]
		id := coinID(f.Code)
		switch {
		case coinIDs[f.Code] == f.ID:
			f.Status = "builtin"
		case id == f.ID && slices.Contains(mine, f.Code):
			f.Status = "tracked"
		case id == f.ID:
			f.Status = ""
		case id != "" || reservedCode(f.Code):
			f.Status = "taken"
			f.TakenBy = names[f.Code]
			if coinIDs[f.Code] != "" {
				f.TakenBy = "встроенная монета " + f.Code
			} else if reservedCode(f.Code) {
				f.TakenBy = "валюта или металл " + f.Code
			}
		}
	}
	return nil
}

// coinMarket — монета по id из /coins/markets: тикер, название, логотип и цена в рублях.
type coinMarket struct {
	ID     string  `json:"id"`
	Symbol string  `json:"symbol"`
	Name   string  `json:"name"`
	Image  string  `json:"image"`
	Price  float64 `json:"current_price"`
}

func (u *RateUpdater) coinMarket(ctx context.Context, id string) (coinMarket, error) {
	body, err := u.get(ctx, fmt.Sprintf(coingeckoMarketsURL, url.QueryEscape(id)))
	if err != nil {
		return coinMarket{}, &APIError{502, "CoinGecko не ответил — попробуйте через минуту"}
	}
	var list []coinMarket
	if err := json.Unmarshal(body, &list); err != nil {
		return coinMarket{}, fmt.Errorf("parse CoinGecko markets: %w", err)
	}
	for _, m := range list {
		if m.ID == id {
			return m, nil
		}
	}
	return coinMarket{}, badRequest("Монета не найдена в CoinGecko")
}

// AddCoin добавляет монету CoinGecko (по id) в список пользователя: заводит
// её на сервере, если её ещё нет, и сразу записывает курс. История цен для
// графиков догружается в фоне.
func (a *API) AddCoin(ctx context.Context, uid int64, in struct {
	ID string `json:"id"`
}) error {
	id := strings.TrimSpace(in.ID)
	if id == "" {
		return badRequest("Выберите монету")
	}
	for code, builtin := range coinIDs {
		if builtin == id {
			return badRequest(code + " уже есть в списке")
		}
	}
	m, err := a.rates.coinMarket(ctx, id)
	if err != nil {
		return err
	}
	code := strings.ToUpper(strings.TrimSpace(m.Symbol))
	if !tickerRe.MatchString(code) {
		return badRequest("У монеты необычный тикер «" + m.Symbol + "» — такую добавить нельзя")
	}
	if coinIDs[code] != "" || reservedCode(code) {
		return badRequest("Тикер " + code + " уже занят — добавить эту монету нельзя")
	}
	if !(m.Price > 0) {
		return badRequest("У CoinGecko нет цены " + m.Name + " в рублях")
	}
	err = pgx.BeginFunc(ctx, a.store.db, func(tx pgx.Tx) error {
		var have string
		err := tx.QueryRow(ctx, `SELECT gecko_id FROM coins WHERE code = $1`, code).Scan(&have)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			var n int
			if err := tx.QueryRow(ctx, `SELECT count(*) FROM coins`).Scan(&n); err != nil {
				return err
			}
			if n >= maxExtraCoins {
				return badRequest("На сервере уже слишком много своих монет")
			}
			if _, err := tx.Exec(ctx, `INSERT INTO coins (code, gecko_id, name, image) VALUES ($1, $2, $3, $4)`,
				code, id, strings.TrimSpace(m.Name), m.Image); err != nil {
				return err
			}
		case err != nil:
			return err
		case have != id:
			return badRequest("Тикер " + code + " уже занят другой монетой")
		}
		_, err = tx.Exec(ctx, `INSERT INTO user_coins (user_id, code) VALUES ($1, $2) ON CONFLICT DO NOTHING`, uid, code)
		return err
	})
	if err != nil {
		return err
	}
	registerCoin(code, id)
	if err := a.store.UpsertRates(ctx, map[string]float64{code: m.Price}, "coingecko"); err != nil {
		return err
	}
	go a.rates.Backfill(context.Background())
	return nil
}

// RemoveCoin убирает свою монету из списка пользователя, если она нигде
// не используется: ни валютой счёта, ни в сделках, ни в портфелях.
func (s *Store) RemoveCoin(ctx context.Context, uid int64, code string) error {
	code = strings.ToUpper(strings.TrimSpace(code))
	var used string
	err := s.db.QueryRow(ctx, `
		SELECT CASE
			WHEN EXISTS (SELECT 1 FROM accounts WHERE user_id = $1 AND currency = $2) THEN 'валюта счёта'
			WHEN EXISTS (SELECT 1 FROM transactions WHERE user_id = $1 AND coin = $2) THEN 'сделки'
			WHEN EXISTS (SELECT 1 FROM crypto_assets WHERE user_id = $1 AND coin = $2) THEN 'портфели'
			ELSE '' END`, uid, code).Scan(&used)
	if err != nil {
		return err
	}
	if used != "" {
		return badRequest(code + " используется (" + used + ") — сначала уберите её оттуда")
	}
	tag, err := s.db.Exec(ctx, `DELETE FROM user_coins WHERE user_id = $1 AND code = $2`, uid, code)
	if err == nil && tag.RowsAffected() == 0 {
		return notFound("Монеты нет в списке")
	}
	return err
}
