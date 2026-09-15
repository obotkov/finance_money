package main

import "testing"

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
