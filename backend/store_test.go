package main

import "testing"

func TestParseImport(t *testing.T) {
	text := "дата;назначение;категория;счёт;сумма;валюта;тип;на счёт\n" +
		"2026-09-14;Пятёрочка;Продукты;Тинькофф Black;-2340;RUB;расход;\r\n" +
		"2026-09-10;Зарплата;Зарплата;Тинькофф Black;142 000,50;RUB;доход;\n" +
		"2026-09-05;На накопительный;Перевод;Тинькофф Black;40000;RUB;Перевод;Накопительный счёт\n" +
		"мусор\n" +
		"2026-09-01;Ноль;Продукты;Наличные;0\n" +
		"2026-09-02\tТаб\tКафе\tНаличные\t−620\n"

	got := parseImport(text)
	want := []importLine{
		{2, TxInput{Date: "2026-09-14", Title: "Пятёрочка", Type: "expense", Category: "Продукты", Account: "Тинькофф Black", Amount: 2340}},
		{3, TxInput{Date: "2026-09-10", Title: "Зарплата", Type: "income", Category: "Зарплата", Account: "Тинькофф Black", Amount: 142000.5}},
		{4, TxInput{Date: "2026-09-05", Title: "На накопительный", Type: "transfer", Account: "Тинькофф Black", ToAccount: "Накопительный счёт", Amount: 40000}},
		{7, TxInput{Date: "2026-09-02", Title: "Таб", Type: "expense", Category: "Кафе", Account: "Наличные", Amount: 620}},
	}
	if len(got) != len(want) {
		t.Fatalf("got %d lines, want %d: %+v", len(got), len(want), got)
	}
	for i := range want {
		if got[i].n != want[i].n || got[i].in != want[i].in {
			t.Errorf("line %d:\n got %+v\nwant %+v", i, got[i], want[i])
		}
	}
}

func TestTxInputValidate(t *testing.T) {
	cases := []struct {
		name string
		in   TxInput
		ok   bool
	}{
		{"expense", TxInput{Date: "2026-09-14", Type: "expense", Category: "Продукты", Account: "A", Amount: 10}, true},
		{"bad date", TxInput{Date: "14.09.2026", Type: "expense", Category: "Продукты", Account: "A", Amount: 10}, false},
		{"zero amount", TxInput{Date: "2026-09-14", Type: "income", Category: "Зарплата", Account: "A"}, false},
		{"no category", TxInput{Date: "2026-09-14", Type: "expense", Account: "A", Amount: 10}, false},
		{"same accounts", TxInput{Date: "2026-09-14", Type: "transfer", Account: "A", ToAccount: "A", Amount: 10}, false},
		{"transfer", TxInput{Date: "2026-09-14", Type: "transfer", Account: "A", ToAccount: "B", Amount: 10}, true},
		{"unknown type", TxInput{Date: "2026-09-14", Type: "gift", Account: "A", Amount: 10}, false},
	}
	for _, c := range cases {
		err := c.in.validate()
		if (err == nil) != c.ok {
			t.Errorf("%s: err = %v, want ok = %v", c.name, err, c.ok)
		}
	}
	in := TxInput{Date: "2026-09-14", Type: "expense", Category: "Продукты", Account: "A", Amount: 10}
	_ = in.validate()
	if in.Title != "Продукты" {
		t.Errorf("empty title should default to the category, got %q", in.Title)
	}
}
