package main

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseImport(t *testing.T) {
	text := "Дата;Сумма (откуда);Счёт (откуда);Сумма (куда);Счёт (куда);Баланс;Категория;Описание;Тип\n" +
		"2026-09-14;2 340,50;Тинькофф Black;;;81 959,50;Продукты;Пятёрочка;расход\r\n" +
		"10.09.2026;;;142000;Тинькофф Black;84 300;Зарплата;Зарплата за сентябрь;Доход\n" +
		"2026-09-05;8433,63;Тинькофф Black;100;Валютный вклад;;;Покупка долларов;перевод\n" +
		"\n" +
		"5.9.2026;−4500 ₽;Наличные;;;;Спорт : Футбол;\"Секция; сентябрь\";\n" +
		"Итого;;;;;;;;\n" +
		"2026-09-01;300;Наличные;;;;;Кофе;расход\n"

	got, err := parseImport(text)
	if err != nil {
		t.Fatal(err)
	}
	received := 100.0
	want := []importLine{
		{2, TxInput{Date: "2026-09-14", Title: "Пятёрочка", Type: "expense", Category: "Продукты", Account: "Тинькофф Black", Amount: 2340.5}, []string{"Продукты"}},
		{3, TxInput{Date: "2026-09-10", Title: "Зарплата за сентябрь", Type: "income", Category: "Зарплата", Account: "Тинькофф Black", Amount: 142000}, []string{"Зарплата"}},
		{4, TxInput{Date: "2026-09-05", Title: "Покупка долларов", Type: "transfer", Account: "Тинькофф Black", ToAccount: "Валютный вклад", Amount: 8433.63, Received: &received}, nil},
		{6, TxInput{Date: "2026-09-05", Title: "Секция; сентябрь", Type: "expense", Category: "Футбол", Account: "Наличные", Amount: 4500}, []string{"Спорт", "Футбол"}},
		{8, TxInput{Date: "2026-09-01", Title: "Кофе", Type: "expense", Category: uncategorized, Account: "Наличные", Amount: 300}, []string{uncategorized}},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d lines, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if !reflect.DeepEqual(got[i], want[i]) {
			t.Errorf("line %d:\n got %+v (received %v)\nwant %+v", i, got[i], deref(got[i].in.Received), want[i])
		}
	}
}

func deref(p *float64) any {
	if p == nil {
		return nil
	}
	return *p
}

func TestParseImportTabs(t *testing.T) {
	got, err := parseImport("2026-09-14\t100\tКарта\t\t\t\tЕда\tОбед\tрасход\n")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].in.Account != "Карта" || got[0].in.Amount != 100 || got[0].in.Category != "Еда" {
		t.Fatalf("got %+v", got)
	}
}

func TestParseImportErrors(t *testing.T) {
	cases := map[string]string{
		"2026-09-14;;Карта;;;;Продукты;x;расход":     "Строка 1: у расхода",
		"2026-09-14;;;;;;Зарплата;x;доход":           "у дохода",
		"2026-09-14;100;Карта;;;;;x;перевод":         "у перевода",
		"2026-09-14;100;Карта;;;;Еда;x;подарок":      "тип «подарок»",
		"2026-09-14;сто;Карта;;;;Еда;x;расход":       "не число",
		"2026-09-14;100;Карта;;;;a:b:c:d:e;x;расход": "четырёх уровней",
		"2026-09-14;;;;;;Еда;x;":                     "ни тип, ни счета",
		"Дата;Сумма\nзаметка":                        "", // no operations at all is not a parse error
	}
	for text, want := range cases {
		_, err := parseImport(text)
		if want == "" {
			if err != nil {
				t.Errorf("%q: unexpected error %v", text, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%q: err = %v, want it to mention %q", text, err, want)
		}
	}
}

func TestParseImportTime(t *testing.T) {
	got, err := parseImport("2026-09-15 14:30;100;Карта;;;;Еда;a;расход\n" +
		"15.09.2026 9:05:07;100;Карта;;;;Еда;b;расход\n" +
		"2026-09-15T08:00;100;Карта;;;;Еда;c;расход\n" +
		"2026-09-14;100;Карта;;;;Еда;d;расход\n")
	if err != nil {
		t.Fatal(err)
	}
	want := [][2]string{{"2026-09-15", "14:30:00"}, {"2026-09-15", "09:05:07"}, {"2026-09-15", "08:00:00"}, {"2026-09-14", ""}}
	for i, w := range want {
		if got[i].in.Date != w[0] || got[i].in.Time != w[1] {
			t.Errorf("line %d: got %s %q, want %s %q", i+1, got[i].in.Date, got[i].in.Time, w[0], w[1])
		}
	}
	if _, err := parseImport("2026-09-15 25:99;100;Карта;;;;Еда;x;расход"); err == nil || !strings.Contains(err.Error(), "Строка 1: время «25:99»") {
		t.Errorf("bad time: err = %v", err)
	}
}

func TestParseAmount(t *testing.T) {
	for in, want := range map[string]float64{"": 0, "1 234,56": 1234.56, "1234.56": 1234.56, "−620": 620, "1 000 ₽": 1000, "0,00014": 0.00014} {
		got, err := parseAmount(in)
		if err != nil || got != want {
			t.Errorf("parseAmount(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	if _, err := parseAmount("abc"); err == nil {
		t.Error(`parseAmount("abc") should fail`)
	}
}

func TestCategoryPath(t *testing.T) {
	if got := categoryPath(" Спорт : Футбол :"); !reflect.DeepEqual(got, []string{"Спорт", "Футбол"}) {
		t.Errorf("got %q", got)
	}
	if got := categoryPath(""); got != nil {
		t.Errorf("empty path: got %q", got)
	}
}
