package main

import (
	"testing"
	"time"
)

func day(s string) time.Time {
	t, err := time.Parse(time.DateOnly, s)
	if err != nil {
		panic(err)
	}
	return t
}

func TestOccurrence(t *testing.T) {
	for _, c := range []struct {
		start, freq string
		n           int
		want        string
	}{
		{"2026-01-31", "month", 1, "2026-02-28"},
		{"2026-01-31", "month", 2, "2026-03-31"},
		{"2026-01-31", "month", 3, "2026-04-30"},
		{"2024-02-29", "year", 1, "2025-02-28"},
		{"2024-02-29", "year", 4, "2028-02-29"},
		{"2026-11-30", "quarter", 1, "2027-02-28"},
		{"2026-09-29", "2weeks", 2, "2026-10-27"},
		{"2026-09-29", "week", 1, "2026-10-06"},
		{"2026-12-31", "day", 1, "2027-01-01"},
	} {
		if got := occurrence(day(c.start), c.freq, c.n).Format(time.DateOnly); got != c.want {
			t.Errorf("%s %s ×%d = %s, want %s", c.start, c.freq, c.n, got, c.want)
		}
	}
}

func TestNextAfter(t *testing.T) {
	for _, c := range []struct{ start, freq, after, want string }{
		{"2026-10-01", "month", "2026-09-29", "2026-10-01"}, // начало ещё впереди
		{"2026-01-31", "month", "2026-02-28", "2026-03-31"},
		{"2026-01-31", "month", "2026-03-01", "2026-03-31"},
		{"2026-09-01", "week", "2026-09-29", "2026-10-06"},
		{"2026-09-01", "day", "2026-09-29", "2026-09-30"},
		{"2020-01-01", "day", "2026-09-29", "2026-09-30"},
		{"2026-09-29", "year", "2026-09-29", "2027-09-29"},
	} {
		if got := nextAfter(day(c.start), c.freq, day(c.after)).Format(time.DateOnly); got != c.want {
			t.Errorf("after %s (%s from %s) = %s, want %s", c.after, c.freq, c.start, got, c.want)
		}
	}
}

func TestDueCount(t *testing.T) {
	s := day("2026-09-01")
	if n := dueCount(s, "week", s, day("2026-09-29")); n != 5 { // 1, 8, 15, 22, 29
		t.Errorf("weeks = %d", n)
	}
	if n := dueCount(s, "month", s, day("2026-08-31")); n != 0 {
		t.Errorf("future = %d", n)
	}
	if n := dueCount(day("2020-01-01"), "day", day("2020-01-01"), day("2026-09-29")); n <= maxCatchUp {
		t.Errorf("years of days = %d", n)
	}
}

func TestRecurringValidate(t *testing.T) {
	ok := RecurringInput{Type: "expense", Account: "Карта", Amount: 100, Category: "Подписки", Freq: "month", Start: "2026-09-29"}
	if _, err := ok.validate(); err != nil {
		t.Fatal(err)
	}
	for name, in := range map[string]RecurringInput{
		"type":     {Type: "transfer", Account: "Карта", Amount: 1, Category: "x", Freq: "month", Start: "2026-09-29"},
		"amount":   {Type: "income", Account: "Карта", Amount: 0, Category: "x", Freq: "month", Start: "2026-09-29"},
		"freq":     {Type: "income", Account: "Карта", Amount: 1, Category: "x", Freq: "hour", Start: "2026-09-29"},
		"start":    {Type: "income", Account: "Карта", Amount: 1, Category: "x", Freq: "day", Start: "29.09.2026"},
		"category": {Type: "income", Account: "Карта", Amount: 1, Freq: "day", Start: "2026-09-29"},
		"account":  {Type: "income", Amount: 1, Category: "x", Freq: "day", Start: "2026-09-29"},
	} {
		if _, err := in.validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
