package main

import "testing"

func TestBudgetLevel(t *testing.T) {
	for _, c := range []struct {
		spent, limit float64
		pct          int
		want         string
	}{
		{0, 1000, 90, ""},
		{899, 1000, 90, ""},
		{900, 1000, 90, "warn"},
		{1000, 1000, 90, "warn"},
		{1000.01, 1000, 90, "over"},
		{1000, 1000, 100, "warn"},
		{500, 1000, 50, "warn"},
		{10, 0, 90, ""},
	} {
		if got := budgetLevel(c.spent, c.limit, c.pct); got != c.want {
			t.Errorf("%v of %v at %d%% = %q, want %q", c.spent, c.limit, c.pct, got, c.want)
		}
	}
}

func TestBudgetMessage(t *testing.T) {
	m := budgetMessage(budgetUsage{ID: 7, Category: "Продукты", Limit: 25000, Spent: 23100.4}, "warn")
	if m.Title != "Продукты: 92 % лимита" || m.Body != "Потрачено 23 100 ₽ из 25 000 ₽, осталось 1 900 ₽ до конца месяца." || m.Tag != "budget-7" {
		t.Errorf("warn: %+v", m)
	}
	m = budgetMessage(budgetUsage{ID: 7, Category: "Продукты", Limit: 25000, Spent: 1250000}, "over")
	if m.Title != "Продукты: лимит превышен" || m.Body != "Потрачено 1 250 000 ₽ из 25 000 ₽ — на 1 225 000 ₽ больше." {
		t.Errorf("over: %+v", m)
	}
	if rub(-1234.6) != "−1 235 ₽" || rub(999) != "999 ₽" {
		t.Errorf("rub: %q %q", rub(-1234.6), rub(999))
	}
}
