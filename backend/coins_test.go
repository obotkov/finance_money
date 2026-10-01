package main

import "testing"

func TestParseCoinGeckoSearch(t *testing.T) {
	body := `{"coins":[
		{"id":"hyperliquid","name":"Hyperliquid","symbol":"HYPE","market_cap_rank":12,"thumb":"https://x/thumb.png"},
		{"id":"weird","name":"Weird","symbol":"$W!","market_cap_rank":0,"thumb":""},
		{"id":"","name":"No id","symbol":"NOID"},
		{"id":"hype-2","name":"Hype Two","symbol":" hype ","market_cap_rank":0,"thumb":""}
	]}`
	got, err := parseCoinGeckoSearch([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 coins (bad ticker and empty id skipped), got %+v", got)
	}
	if got[0].Code != "HYPE" || got[0].ID != "hyperliquid" || got[0].Rank != 12 {
		t.Errorf("first: %+v", got[0])
	}
	if got[1].Code != "HYPE" || got[1].ID != "hype-2" {
		t.Errorf("second (ticker trimmed, upper-cased): %+v", got[1])
	}
	if _, err := parseCoinGeckoSearch([]byte(`not json`)); err == nil {
		t.Error("broken JSON must fail")
	}
}

func TestReservedAndRegisteredCodes(t *testing.T) {
	for _, code := range []string{"RUB", "USD", "CNY", "XAU", "XCU"} {
		if !reservedCode(code) {
			t.Errorf("%s must be reserved", code)
		}
	}
	if reservedCode("HYPE") {
		t.Error("HYPE is not reserved")
	}
	if coinID("ZZTEST") != "" || isCurrency("ZZTEST") {
		t.Fatal("unknown coin must not be a currency")
	}
	registerCoin("ZZTEST", "zz-test")
	defer func() {
		extraCoins.Lock()
		delete(extraCoins.ids, "ZZTEST")
		extraCoins.Unlock()
	}()
	if coinID("ZZTEST") != "zz-test" || !isCurrency("ZZTEST") || allCoinIDs()["ZZTEST"] != "zz-test" {
		t.Error("registered coin must be known everywhere")
	}
	if allCoinIDs()["BTC"] != "bitcoin" {
		t.Error("builtin coins stay in the list")
	}
}
