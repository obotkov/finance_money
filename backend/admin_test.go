package main

import (
	"strings"
	"testing"
	"time"
)

func TestPrintUsers(t *testing.T) {
	login := time.Date(2026, 9, 15, 7, 30, 0, 0, time.UTC)
	users := []UserInfo{
		{User: User{ID: 1, Email: "anna@example.com", Name: "Анна"}, Password: true, Accounts: 2, Txs: 10,
			CreatedAt: time.Date(2026, 9, 15, 7, 0, 0, 0, time.UTC), LastLoginAt: &login},
		{User: User{ID: 2, Email: "boris@gmail.com", Name: "Борис"}, Password: true, Google: true,
			CreatedAt: time.Date(2026, 9, 15, 8, 0, 0, 0, time.UTC)},
		{User: User{ID: 3, Email: "vera@gmail.com", Name: "Вера"}, Google: true,
			CreatedAt: time.Date(2026, 9, 15, 9, 0, 0, 0, time.UTC)},
	}
	var b strings.Builder
	printUsers(&b, users)
	out := b.String()
	for _, want := range []string{"anna@example.com", "2026-09-15 07:30", "пароль, ждёт Google", "Всего: 3"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	lines := strings.Split(out, "\n")
	if !strings.Contains(lines[3], "Google") || strings.Contains(lines[3], "пароль") {
		t.Errorf("Google-only user shown wrong: %q", lines[3])
	}
	if !strings.Contains(lines[2], "—") {
		t.Errorf("user without sign-ins should show —: %q", lines[2])
	}

	b.Reset()
	printUsers(&b, nil)
	if b.String() != "Пользователей нет\n" {
		t.Errorf("empty list: %q", b.String())
	}
}
