package main

import (
	"strings"
	"testing"
)

func TestParseBackup(t *testing.T) {
	ok := `{"format":"penny-backup","version":1,
		"accounts":[{"id":1,"name":"Карта","kind":"Карта","currency":"RUB","balance":"100.5"}],
		"categories":[{"id":10,"name":"Спорт","kind":"expense"},{"id":11,"name":"Футбол","parent":10,"kind":"expense"}],
		"transactions":[{"id":5,"date":"2026-09-01","title":"Мяч","type":"expense","categoryId":11,"accountId":1,"amount":"100"}]}`
	f, err := parseBackup([]byte(ok))
	if err != nil {
		t.Fatal(err)
	}
	if len(f.Accounts) != 1 || len(f.Categories) != 2 || len(f.Transactions) != 1 || f.Accounts[0].Balance != "100.5" {
		t.Fatalf("parsed %+v", f)
	}

	for name, c := range map[string]struct{ body, want string }{
		"not json":       {`hello`, "не файл бекапа"},
		"other format":   {`{"format":"x","version":1}`, "не файл бекапа"},
		"version":        {`{"format":"penny-backup","version":7}`, "Версия бекапа 7"},
		"currency":       {`{"format":"penny-backup","version":1,"accounts":[{"id":1,"name":"A","currency":"XYZ"}]}`, "неизвестная валюта XYZ"},
		"missing acc":    {`{"format":"penny-backup","version":1,"transactions":[{"title":"x","accountId":3}]}`, "ссылается на счёт"},
		"missing to":     {`{"format":"penny-backup","version":1,"accounts":[{"id":1,"name":"A","currency":"RUB"}],"transactions":[{"title":"x","accountId":1,"toAccountId":2}]}`, "ссылается на счёт"},
		"no parent":      {`{"format":"penny-backup","version":1,"categories":[{"id":1,"name":"A","parent":9}]}`, "нет родителя"},
		"parent cycle":   {`{"format":"penny-backup","version":1,"categories":[{"id":1,"name":"A","parent":2},{"id":2,"name":"B","parent":1}]}`, "вложенность"},
		"too deep":       {`{"format":"penny-backup","version":1,"categories":[{"id":1,"name":"A"},{"id":2,"name":"B","parent":1},{"id":3,"name":"C","parent":2},{"id":4,"name":"D","parent":3},{"id":5,"name":"E","parent":4}]}`, "вложенность"},
		"empty acc name": {`{"format":"penny-backup","version":1,"accounts":[{"id":1,"name":" ","currency":"RUB"}]}`, "без названия"},
	} {
		_, err := parseBackup([]byte(c.body))
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: got %v, want %q", name, err, c.want)
		}
	}
}
