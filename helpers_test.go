package main

import (
	"crypto/sha256"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestNewID(t *testing.T) {
	if a, b := newID(), newID(); a == b || len(a) != 16 || !validID(a) || strings.ContainsAny(a, "0189") {
		t.Fatalf("newID: %q %q", a, b)
	}
}

func TestParseOption(t *testing.T) {
	loc, _ := time.LoadLocation("Europe/Vienna")
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, loc)
	const none = ""
	for in, want := range map[string]string{
		"Mo, 12.10.2026, 14:00–15:30":        "2026-10-12 14:00 – 2026-10-12 15:30",
		"Fr, 30.10.2026, 22:00-01:00":        "2026-10-30 22:00 – 2026-10-31 01:00",
		"Mon, 12.10.2026, 9:00":              "2026-10-12 09:00 – 2026-10-12 10:00",
		"Di, 13.10.2026":                     "allday 2026-10-13",
		"lun., 12.10.2026, 18:00–21:00":      "2026-10-12 18:00 – 2026-10-12 21:00",
		"2026-10-12 18:00":                   "2026-10-12 18:00 – 2026-10-12 19:00",
		"2026-10-12T18:00-21:30":             "2026-10-12 18:00 – 2026-10-12 21:30",
		"Party am 12.10.26 um 18.00 Uhr":     "2026-10-12 18:00 – 2026-10-12 19:00",
		"12.10. 18:00 bis 21:00":             "2026-10-12 18:00 – 2026-10-12 21:00",
		"BBQ 03.10.":                         "allday 2027-10-03",
		"Saturday 2026-11-14 10:00 to 12:00": "2026-11-14 10:00 – 2026-11-14 12:00",
		"Pizza":                              none,
		"31.02.2026":                         none,
		"12.10.2026, 25:00":                  none,
		"Version 1.2.3":                      none,
	} {
		o := parseOption(in, loc, now)
		got := none
		switch {
		case o.AllDay:
			got = "allday " + o.Start.Format("2006-01-02")
		case o.HasDate():
			got = o.Start.Format("2006-01-02 15:04") + " – " + o.End.Format("2006-01-02 15:04")
		}
		if got != want {
			t.Errorf("parseOption(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestICS(t *testing.T) {
	loc, _ := time.LoadLocation("Europe/Vienna")
	now := time.Date(2026, 10, 8, 10, 0, 0, 0, time.UTC)
	ics := string(buildICS("x@host", "Dinner; with, friends", strings.Repeat("ö", 60), parseOption("12.10.2026, 14:00–15:30", loc, now), now))
	for _, want := range []string{"BEGIN:VCALENDAR\r\n", "DTSTART:20261012T120000Z\r\n", "DTEND:20261012T133000Z\r\n", `SUMMARY:Dinner\; with\, friends`, "DTSTAMP:20261008T100000Z"} {
		if !strings.Contains(ics, want) {
			t.Errorf("missing %q in\n%s", want, ics)
		}
	}
	for _, l := range strings.Split(ics, "\r\n") {
		if len(l) > 75 {
			t.Errorf("unfolded line (%d): %q", len(l), l)
		}
	}
	if !strings.Contains(string(buildICS("x", "T", "", parseOption("13.10.2026", loc, now), now)), "DTSTART;VALUE=DATE:20261013\r\nDTEND;VALUE=DATE:20261014") {
		t.Error("all-day ICS")
	}
}

func solveForTest(challenge string, bits int) string {
	for n := 0; ; n++ {
		s := sha256.Sum256([]byte(challenge + strconv.Itoa(n)))
		if leadingZeroBits(s[:]) >= bits {
			return strconv.Itoa(n)
		}
	}
}

func TestPowVerify(t *testing.T) {
	now := time.Now()
	clock := func() time.Time { return now }
	p := newPow(12, clock)
	ch := p.issue(0)
	good := solveForTest(ch, 12)
	if !p.verify(ch, good) {
		t.Fatal("valid solution rejected")
	}
	if p.verify(ch, good) {
		t.Fatal("replay accepted")
	}

	ch = p.issue(0)
	for n := 0; ; n++ {
		s := sha256.Sum256([]byte(ch + strconv.Itoa(n)))
		if leadingZeroBits(s[:]) < 12 {
			if p.verify(ch, strconv.Itoa(n)) {
				t.Fatal("too-easy solution accepted")
			}
			break
		}
	}

	other := newPow(12, clock)
	ch = other.issue(0)
	if p.verify(ch, solveForTest(ch, 12)) {
		t.Fatal("foreign signature accepted")
	}
	tampered := "1" + p.issue(0)
	if p.verify(tampered, solveForTest(tampered, 12)) {
		t.Fatal("tampered challenge accepted")
	}

	ch = p.issue(0)
	sol := solveForTest(ch, 12)
	now = now.Add(powTTL + time.Second)
	if p.verify(ch, sol) {
		t.Fatal("expired challenge accepted")
	}
	ch = p.issue(0)
	if !p.verify(ch, solveForTest(ch, 12)) || len(p.used) != 1 {
		t.Fatalf("expired replay entries not pruned: %d", len(p.used))
	}
}

func TestDetectLang(t *testing.T) {
	for in, want := range map[string]string{
		"":                        "en",
		"de-AT,de;q=0.9,en;q=0.8": "de",
		"fr-FR,fr;q=0.9,de;q=0.5": "fr",
		"es-ES,es;q=0.9":          "es",
		"it-IT,it;q=0.9,de;q=0.5": "de",
		"*":                       "en",
		"de;q=0":                  "en",
	} {
		if got := detectLang(in); got != want {
			t.Errorf("detectLang(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestConfig(t *testing.T) {
	for _, k := range []string{"BASE_URL", "ADMIN_PASSWORD", "SMTP_URL", "SMTP_FROM", "THEME", "PUBLIC_CREATE"} {
		t.Setenv(k, "")
	}
	t.Setenv("BASE_URL", "https://poll.example/")
	t.Setenv("ADMIN_PASSWORD", "long secret")
	t.Setenv("SMTP_URL", "smtp://me%40example.com:p%2Fss@mail.example:587")
	t.Setenv("SMTP_FROM", "poll@example.com")
	s, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if s.BaseURL != "https://poll.example" || s.SMTP != (smtpConfig{Addr: "mail.example:587", User: "me@example.com", Password: "p/ss", From: "poll@example.com", TLS: "starttls"}) || s.Theme != "minimal" {
		t.Fatalf("%+v", s)
	}
	t.Setenv("ADMIN_PASSWORD", "admin")
	t.Setenv("BASE_URL", "http://localhost:8737")
	t.Setenv("SMTP_URL", "smtp+insecure://localhost:1025")
	if s, err := loadConfig(); err != nil || s.SMTP.TLS != "none" || s.SMTP.User != "" {
		t.Fatalf("smtp+insecure means no TLS: %v %+v", err, s.SMTP)
	}
	t.Setenv("THEME", "rainbow")
	t.Setenv("SMTP_FROM", "nope")
	t.Setenv("SMTP_URL", "smtps://secret@mail.example")
	_, err = loadConfig()
	for _, want := range []string{"SMTP_FROM", "SMTP_URL", "THEME must be one of business, elegant, fun, minimal"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("want error about %s, got %v", want, err)
		}
	}
	if strings.Contains(err.Error(), "secret") {
		t.Error("SMTP_URL errors must not echo credentials")
	}
}
