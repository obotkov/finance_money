package main

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestValidateCredentials(t *testing.T) {
	cases := []struct {
		email, password string
		ok              bool
	}{
		{"user@example.com", "password1", true},
		{"user@example.com", "пароль12", true}, // 8 characters, 16 bytes
		{"user@example", "password1", false},
		{"user.example.com", "password1", false},
		{"name <user@example.com>", "password1", false},
		{"user@example.com", "short", false},
		{"user@example.com", strings.Repeat("я", 40), false}, // 80 bytes
	}
	for _, c := range cases {
		if err := validateCredentials(c.email, c.password); (err == nil) != c.ok {
			t.Errorf("%q / %q: err = %v, want ok = %v", c.email, c.password, err, c.ok)
		}
	}
}

func TestNormalizeEmail(t *testing.T) {
	if got := normalizeEmail("  User@Example.COM "); got != "user@example.com" {
		t.Errorf("got %q", got)
	}
}

func TestLimiter(t *testing.T) {
	l := newLimiter(time.Minute)
	for range 3 {
		l.add("a")
	}
	if l.allow("a", 3) {
		t.Error("3 hits of 3 allowed")
	}
	if !l.allow("a", 4) || !l.allow("b", 1) {
		t.Error("under the limit refused")
	}

	l.hits["a"][0] = time.Now().Add(-2 * time.Minute) // one hit leaves the window
	if !l.allow("a", 3) {
		t.Error("expired hit still counted")
	}

	l.reset("a")
	if len(l.hits["a"]) != 0 {
		t.Error("reset kept hits")
	}
}

func TestClientIP(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "172.18.0.4:51234"
	if got := clientIP(r); got != "172.18.0.4" {
		t.Errorf("without XFF: %q", got)
	}
	r.Header.Set("X-Forwarded-For", "203.0.113.7, 172.18.0.4")
	if got := clientIP(r); got != "203.0.113.7" {
		t.Errorf("with XFF: %q", got)
	}
}
