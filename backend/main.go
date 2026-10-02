// Command api is the backend of Penny: users with email/password or
// Google sign-in, their accounts, operations and categories in Postgres, plus
// exchange rates refreshed from the CBR and CoinGecko.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

func main() {
	if len(os.Args) > 1 {
		switch os.Args[1] {
		case "healthcheck":
			os.Exit(healthcheck())
		case "users", "reset-password", "help":
			os.Exit(runAdmin(os.Args[1:]))
		}
	}
	log := slog.New(slog.NewTextHandler(os.Stdout, nil))
	if err := run(log); err != nil {
		log.Error("fatal", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	store, err := OpenStore(ctx, databaseURL(), log)
	if err != nil {
		return err
	}
	defer store.Close()

	// свои монеты — до первого обновления курсов, чтобы оно их уже знало
	if err := store.LoadCoins(ctx); err != nil {
		return err
	}
	rates := NewRateUpdater(store, log)
	go rates.Run(ctx, time.Hour)

	cfg := Config{
		PublicURL:          strings.TrimRight(getenv("PUBLIC_URL", "http://localhost"), "/"),
		GoogleClientID:     os.Getenv("GOOGLE_CLIENT_ID"),
		GoogleClientSecret: os.Getenv("GOOGLE_CLIENT_SECRET"),
	}
	push, err := NewPusher(ctx, store, cfg.PublicURL, log)
	if err != nil {
		return err
	}
	go store.RunRecurring(ctx, log, 15*time.Minute, func(uid int64) { push.CheckBudgets(uid) })
	// Telegram-бот: сводка и бекапы; без токена выключен
	tg := NewTelegram(store, os.Getenv("TELEGRAM_BOT_TOKEN"), os.Getenv("TELEGRAM_API_URL"), log)
	if tg != nil {
		go tg.Run(ctx)
	}
	api := NewAPI(store, rates, push, cfg, log)
	api.tg = tg
	log.Info("config", "public_url", cfg.PublicURL, "google_sign_in", api.google != nil, "telegram", tg != nil)

	srv := &http.Server{
		Addr:              getenv("ADDR", ":8080"),
		Handler:           api.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()

	log.Info("listening", "addr", srv.Addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// healthcheck backs the container's HEALTHCHECK: the distroless image has no curl.
func healthcheck() int {
	client := http.Client{Timeout: 3 * time.Second}
	resp, err := client.Get("http://127.0.0.1" + getenv("ADDR", ":8080") + "/api/health")
	if err != nil {
		return 1
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}

func databaseURL() string {
	return getenv("DATABASE_URL", "postgres://finance:finance@localhost:5432/finance?sslmode=disable")
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
