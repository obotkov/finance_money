package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"text/tabwriter"
)

const adminUsage = `Команды администратора (на сервере, из /opt/finance-money):
  docker compose exec api /api users    список пользователей

Вход только через Google, паролей нет.`

// runAdmin runs an admin subcommand against the database and returns the exit code.
func runAdmin(args []string) int {
	if args[0] != "users" || len(args) != 1 {
		fmt.Fprintln(os.Stderr, adminUsage)
		return 2
	}

	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))
	store, err := OpenStore(ctx, databaseURL(), log)
	if err != nil {
		fmt.Fprintln(os.Stderr, "ошибка:", err)
		return 1
	}
	defer store.Close()

	list, err := store.ListUsers(ctx)
	if err == nil {
		printUsers(os.Stdout, list)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "ошибка:", err)
		return 1
	}
	return 0
}

func printUsers(w io.Writer, users []UserInfo) {
	if len(users) == 0 {
		fmt.Fprintln(w, "Пользователей нет")
		return
	}
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tПОЧТА\tИМЯ\tВХОД\tСЧЕТОВ\tОПЕРАЦИЙ\tРЕГИСТРАЦИЯ (UTC)\tПОСЛЕДНИЙ ВХОД (UTC)")
	for _, u := range users {
		// аккаунт со старым паролем без Google войдёт, когда владелец зайдёт через Google с той же почтой
		method := "Google"
		if !u.Google {
			method = "пароль, ждёт Google"
		}
		last := "—"
		if u.LastLoginAt != nil {
			last = u.LastLoginAt.UTC().Format("2006-01-02 15:04")
		}
		fmt.Fprintf(tw, "%d\t%s\t%s\t%s\t%d\t%d\t%s\t%s\n",
			u.ID, u.Email, u.Name, method, u.Accounts, u.Txs, u.CreatedAt.UTC().Format("2006-01-02 15:04"), last)
	}
	tw.Flush()
	fmt.Fprintf(w, "Всего: %d\n", len(users))
}
