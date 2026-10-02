package main

import "testing"

func TestWidgetValidate(t *testing.T) {
	ok := []Widget{
		{Source: "all", Size: "wide", Rows: 4, Cur: "USDT"},
		{Source: "p:12", Size: "small", Rows: 0, Cur: "RUB"},
		{Source: "a:7", Size: "small", Rows: 10, Cur: "USDT"},
	}
	for _, w := range ok {
		if err := w.validate(); err != nil {
			t.Errorf("%+v: %v", w, err)
		}
	}
	bad := []Widget{
		{Source: "x:1", Size: "small", Rows: 4, Cur: "USDT"},
		{Source: "p:", Size: "small", Rows: 4, Cur: "USDT"},
		{Source: "a:-3", Size: "small", Rows: 4, Cur: "USDT"},
		{Source: "all", Size: "huge", Rows: 4, Cur: "USDT"},
		{Source: "all", Size: "small", Rows: 11, Cur: "USDT"},
		{Source: "all", Size: "small", Rows: 4, Cur: "EUR"},
	}
	for _, w := range bad {
		if err := w.validate(); err == nil {
			t.Errorf("%+v: ждали ошибку", w)
		}
	}
}
