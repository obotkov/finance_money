package main

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestSummaryRange(t *testing.T) {
	day := func(s string) time.Time { d, _ := time.Parse(time.DateOnly, s); return d }
	// понедельник 5 октября 2026: по расписанию — прошлая неделя целиком
	from, to := summaryRange(day("2026-10-05"), true)
	if from != day("2026-09-28") || to != day("2026-10-04") {
		t.Errorf("weekly on Monday: %v — %v", from, to)
	}
	// среда (догнали рассылку): всё равно прошлая неделя
	from, to = summaryRange(day("2026-10-07"), true)
	if from != day("2026-09-28") || to != day("2026-10-04") {
		t.Errorf("weekly on Wednesday: %v — %v", from, to)
	}
	// по кнопке — последние семь дней
	from, to = summaryRange(day("2026-10-07"), false)
	if from != day("2026-10-01") || to != day("2026-10-07") {
		t.Errorf("manual: %v — %v", from, to)
	}
	if got := rangeLabel(day("2026-09-21"), day("2026-09-27")); got != "21–27 сентября" {
		t.Errorf("label in one month: %q", got)
	}
	if got := rangeLabel(day("2026-09-28"), day("2026-10-04")); got != "28 сентября — 4 октября" {
		t.Errorf("label across months: %q", got)
	}
	if got := isoWeek(day("2026-10-05")); got != "2026-W41" {
		t.Errorf("iso week: %q", got)
	}
	if signedRub(1500) != "+1 500 ₽" || signedRub(-20) != "−20 ₽" || signedRub(0) != "0 ₽" {
		t.Errorf("signedRub: %q %q %q", signedRub(1500), signedRub(-20), signedRub(0))
	}
}

func TestStartError(t *testing.T) {
	if got := startError(errors.New("telegram: Unauthorized")); !strings.Contains(got, "TELEGRAM_BOT_TOKEN") {
		t.Errorf("Unauthorized: %q", got)
	}
	if got := startError(errors.New(`Post "https://api.telegram.org/bot<token>/getMe": dial tcp 1.2.3.4:443: i/o timeout`)); !strings.Contains(got, "api.telegram.org") {
		t.Errorf("сеть: %q", got)
	}
}
