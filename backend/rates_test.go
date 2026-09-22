package main

import (
	"math"
	"testing"

	"golang.org/x/text/encoding/charmap"
)

func TestParseCBR(t *testing.T) {
	src := `<?xml version="1.0" encoding="windows-1251"?>
<ValCurs Date="15.09.2026" name="Foreign Currency Market">
<Valute ID="R01235"><NumCode>840</NumCode><CharCode>USD</CharCode><Nominal>1</Nominal><Name>Доллар США</Name><Value>84,1234</Value></Valute>
<Valute ID="R01239"><NumCode>978</NumCode><CharCode>EUR</CharCode><Nominal>1</Nominal><Name>Евро</Name><Value>98,5</Value></Valute>
<Valute ID="R01375"><NumCode>156</NumCode><CharCode>CNY</CharCode><Nominal>10</Nominal><Name>Юань</Name><Value>117,3</Value></Valute>
</ValCurs>`
	body, err := charmap.Windows1251.NewEncoder().String(src)
	if err != nil {
		t.Fatal(err)
	}
	got, err := parseCBR([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]float64{"USD": 84.1234, "EUR": 98.5}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for code, w := range want {
		if math.Abs(got[code]-w) > 1e-9 {
			t.Errorf("%s = %v, want %v", code, got[code], w)
		}
	}
}

func TestParseCBRNominal(t *testing.T) {
	src := `<?xml version="1.0" encoding="windows-1251"?><ValCurs>
<Valute><CharCode>USD</CharCode><Nominal>1</Nominal><Value>80</Value></Valute>
<Valute><CharCode>EUR</CharCode><Nominal>10</Nominal><Value>950,5</Value></Valute></ValCurs>`
	got, err := parseCBR([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	if math.Abs(got["EUR"]-95.05) > 1e-9 {
		t.Errorf("EUR = %v, want 95.05", got["EUR"])
	}
}

func TestParseCBRMissingCurrency(t *testing.T) {
	src := `<?xml version="1.0" encoding="windows-1251"?><ValCurs>
<Valute><CharCode>USD</CharCode><Nominal>1</Nominal><Value>80</Value></Valute></ValCurs>`
	if _, err := parseCBR([]byte(src)); err == nil {
		t.Fatal("want error when EUR is missing")
	}
}

func TestParseCoinGecko(t *testing.T) {
	body := `{"bitcoin":{"rub":6658967},"ethereum":{"rub":215235},"the-open-network":{"rub":114.6},"tether":{"rub":84.48}}`
	got, err := parseCoinGecko([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]float64{"BTC": 6658967, "ETH": 215235, "TON": 114.6, "USDT": 84.48}
	for code, w := range want {
		if got[code] != w {
			t.Errorf("%s = %v, want %v", code, got[code], w)
		}
	}
	if _, err := parseCoinGecko([]byte(`{"bitcoin":{"rub":1}}`)); err == nil {
		t.Error("want error when coins are missing")
	}
}

func TestParseCoinGeckoChart(t *testing.T) {
	// 2026-09-01 00:00 UTC, the same day later on, and 2026-09-02
	body := []byte(`{"prices":[[1788220800000,100.5],[1788260000000,101],[1788307200000,102]]}`)
	got, err := parseCoinGeckoChart(body)
	if err != nil {
		t.Fatal(err)
	}
	if got["2026-09-01"] != 101 || got["2026-09-02"] != 102 || len(got) != 2 {
		t.Fatalf("got %v", got)
	}
}
