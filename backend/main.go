// Command api is the backend of «Ведомость»: users with email/password or
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
	if len(os.Args) > 1 && os.Args[1] == "healthcheck" {
		os.Exit(healthcheck())
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

	store, err := OpenStore(ctx, getenv("DATABASE_URL", "postgres://finance:finance@localhost:5432/finance?sslmode=disable"), log)
	if err != nil {
		return err
	}
	defer store.Close()

	rates := NewRateUpdater(store, log)
	go rates.Run(ctx, time.Hour)

	cfg := Config{
		PublicURL:          strings.TrimRight(getenv("PUBLIC_URL", "http://localhost"), "/"),
		GoogleClientID:     os.Getenv("GOOGLE_CLIENT_ID"),
		GoogleClientSecret: os.Getenv("GOOGLE_CLIENT_SECRET"),
	}
	api := NewAPI(store, rates, cfg, log)
	log.Info("config", "public_url", cfg.PublicURL, "google_sign_in", api.google != nil)

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

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
