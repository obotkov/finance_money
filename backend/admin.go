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
  docker compose exec api /api users                     список пользователей
  docker compose exec api /api reset-password <почта>    новый случайный пароль для пользователя

Пароли хранятся только как bcrypt-хеши, поэтому вывести текущий пароль нельзя — только задать новый.`

// runAdmin runs an admin subcommand against the database and returns the exit code.
func runAdmin(args []string) int {
	users := args[0] == "users" && len(args) == 1
	reset := args[0] == "reset-password" && len(args) == 2
	if !users && !reset {
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

	if users {
		var list []UserInfo
		if list, err = store.ListUsers(ctx); err == nil {
			printUsers(os.Stdout, list)
		}
	} else {
		var password string
		if password, err = store.ResetPassword(ctx, args[1]); err == nil {
			fmt.Printf("Новый пароль для %s: %s\nСтарые сессии этого пользователя завершены.\n", normalizeEmail(args[1]), password)
		}
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
		method := "пароль"
		switch {
		case u.Password && u.Google:
			method = "пароль + Google"
		case u.Google:
			method = "Google"
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
