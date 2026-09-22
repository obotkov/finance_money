package main

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/text/encoding/charmap"
)

const (
	cbrURL       = "https://www.cbr.ru/scripts/XML_daily.asp"
	coingeckoURL = "https://api.coingecko.com/api/v3/simple/price"
	// дневные цены монеты за последние дни: /coins/{id}/market_chart
	coingeckoChartURL = "https://api.coingecko.com/api/v3/coins/%s/market_chart?vs_currency=rub&days=%d&interval=daily"
	userAgent         = "finance-money/1.0 (+https://okayconnect.online)"
)

// Fiat comes from the Bank of Russia, crypto from CoinGecko (by coin id).
// coinIDs is also the list of coins a crypto portfolio may hold; coreCoins are
// the ones an account can be kept in, so a refresh without them is a failure.
// historyDays — сколько дней курсов отдаётся странице для графиков.
const historyDays = 31

var (
	fiatCodes = []string{"USD", "EUR"}
	coreCoins = []string{"BTC", "ETH", "TON", "USDT"}
	coinIDs   = map[string]string{
		"BTC": "bitcoin", "ETH": "ethereum", "TON": "the-open-network", "USDT": "tether",
		"USDC": "usd-coin", "SOL": "solana", "BNB": "binancecoin", "XRP": "ripple",
		"ADA": "cardano", "DOGE": "dogecoin", "TRX": "tron", "AVAX": "avalanche-2",
		"LINK": "chainlink", "DOT": "polkadot", "LTC": "litecoin", "SUI": "sui",
		"NOT": "notcoin", "MATIC": "matic-network",
	}
)

type RateUpdater struct {
	store *Store
	log   *slog.Logger
	http  *http.Client
	mu    sync.Mutex // one refresh at a time
}

func NewRateUpdater(store *Store, log *slog.Logger) *RateUpdater {
	return &RateUpdater{store: store, log: log, http: &http.Client{Timeout: 20 * time.Second}}
}

// Run refreshes the rates now and then every interval until ctx is done.
// After each refresh the daily history of coins is backfilled where it has gaps.
func (u *RateUpdater) Run(ctx context.Context, every time.Duration) {
	_ = u.Refresh(ctx)
	u.Backfill(ctx)
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			_ = u.Refresh(ctx)
			u.Backfill(ctx)
		}
	}
}

// Backfill fetches the daily prices of the last historyDays days for coins
// whose history has gaps (a new server, a new coin). CoinGecko's free API
// allows a few calls a minute, so the calls are spaced out and a refusal
// stops the round — the next one picks up the rest.
func (u *RateUpdater) Backfill(ctx context.Context) {
	have, err := u.store.HistoryDays(ctx)
	if err != nil {
		u.log.Warn("rate history", "err", err)
		return
	}
	codes := make([]string, 0, len(coinIDs))
	for code := range coinIDs {
		if have[code] < historyDays-2 {
			codes = append(codes, code)
		}
	}
	slices.Sort(codes)
	for i, code := range codes {
		if i > 0 {
			select {
			case <-ctx.Done():
				return
			case <-time.After(15 * time.Second):
			}
		}
		body, err := u.get(ctx, fmt.Sprintf(coingeckoChartURL, coinIDs[code], historyDays))
		if err == nil {
			var days map[string]float64
			if days, err = parseCoinGeckoChart(body); err == nil {
				err = u.store.AddHistory(ctx, code, days)
			}
		}
		if err != nil {
			u.log.Warn("rate history backfill stopped", "coin", code, "err", err)
			return
		}
		u.log.Info("rate history backfilled", "coin", code)
	}
}

// parseCoinGeckoChart turns market_chart prices ([ms, price] pairs) into a
// price per UTC day; today is left to the live refresh.
func parseCoinGeckoChart(body []byte) (map[string]float64, error) {
	var resp struct {
		Prices [][2]float64 `json:"prices"`
	}
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("parse CoinGecko chart: %w", err)
	}
	today := time.Now().UTC().Format(time.DateOnly)
	out := map[string]float64{}
	for _, p := range resp.Prices {
		day := time.UnixMilli(int64(p[0])).UTC().Format(time.DateOnly)
		if p[1] > 0 && day < today {
			out[day] = p[1]
		}
	}
	if len(out) == 0 {
		return nil, errors.New("no prices in CoinGecko chart")
	}
	return out, nil
}

// Refresh fetches both sources; a source that fails keeps its previous rates.
func (u *RateUpdater) Refresh(ctx context.Context) error {
	u.mu.Lock()
	defer u.mu.Unlock()

	sources := []struct {
		name  string
		fetch func(context.Context) (map[string]float64, error)
	}{
		{"cbr", u.fetchCBR},
		{"coingecko", u.fetchCoinGecko},
	}
	var errs []error
	for _, src := range sources {
		rates, err := src.fetch(ctx)
		if err == nil {
			err = u.store.UpsertRates(ctx, rates, src.name)
		}
		if err != nil {
			errs = append(errs, fmt.Errorf("%s: %w", src.name, err))
			continue
		}
		u.log.Info("rates refreshed", "source", src.name, "rates", rates)
	}
	err := errors.Join(errs...)
	if err != nil {
		u.log.Warn("rates refresh failed", "err", err)
	}
	return err
}

func (u *RateUpdater) fetchCBR(ctx context.Context) (map[string]float64, error) {
	body, err := u.get(ctx, cbrURL)
	if err != nil {
		return nil, err
	}
	return parseCBR(body)
}

func (u *RateUpdater) fetchCoinGecko(ctx context.Context) (map[string]float64, error) {
	ids := make([]string, 0, len(coinIDs))
	for _, id := range coinIDs {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	body, err := u.get(ctx, coingeckoURL+"?vs_currencies=rub&ids="+strings.Join(ids, ","))
	if err != nil {
		return nil, err
	}
	return parseCoinGecko(body)
}

func (u *RateUpdater) get(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := u.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 2<<20))
}

// parseCBR reads the daily XML of the Bank of Russia (windows-1251,
// "Value" per "Nominal" units with a decimal comma).
func parseCBR(body []byte) (map[string]float64, error) {
	var doc struct {
		Valutes []struct {
			CharCode string `xml:"CharCode"`
			Nominal  string `xml:"Nominal"`
			Value    string `xml:"Value"`
		} `xml:"Valute"`
	}
	dec := xml.NewDecoder(bytes.NewReader(body))
	dec.CharsetReader = func(label string, input io.Reader) (io.Reader, error) {
		if strings.EqualFold(label, "windows-1251") {
			return charmap.Windows1251.NewDecoder().Reader(input), nil
		}
		return nil, fmt.Errorf("unexpected charset %q", label)
	}
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("parse CBR XML: %w", err)
	}
	out := map[string]float64{}
	for _, v := range doc.Valutes {
		if !slices.Contains(fiatCodes, v.CharCode) {
			continue
		}
		value, err := strconv.ParseFloat(strings.Replace(strings.TrimSpace(v.Value), ",", ".", 1), 64)
		if err != nil {
			return nil, fmt.Errorf("%s value %q: %w", v.CharCode, v.Value, err)
		}
		nominal, err := strconv.Atoi(strings.TrimSpace(v.Nominal))
		if err != nil || nominal <= 0 {
			return nil, fmt.Errorf("%s nominal %q", v.CharCode, v.Nominal)
		}
		out[v.CharCode] = value / float64(nominal)
	}
	for _, code := range fiatCodes {
		if out[code] <= 0 {
			return nil, fmt.Errorf("no %s in CBR XML", code)
		}
	}
	return out, nil
}

func parseCoinGecko(body []byte) (map[string]float64, error) {
	var resp map[string]map[string]float64
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, fmt.Errorf("parse CoinGecko JSON: %w", err)
	}
	out := map[string]float64{}
	for code, id := range coinIDs {
		rub := resp[id]["rub"]
		if rub <= 0 {
			if slices.Contains(coreCoins, code) {
				return nil, fmt.Errorf("no RUB price for %s", id)
			}
			continue // a coin CoinGecko didn't price keeps its previous rate
		}
		out[code] = rub
	}
	return out, nil
}
