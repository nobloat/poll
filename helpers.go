package main

import (
	"crypto/rand"
	"encoding/json"
	"fmt"
	"net/mail"
	"path"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

var idRe = regexp.MustCompile(`^[a-z0-9-]{1,64}$`)

func validID(s string) bool { return idRe.MatchString(s) }

func newID() string { return strings.ToLower(rand.Text()[:16]) }

func validEmail(s string) bool {
	a, err := mail.ParseAddress(s)
	return err == nil && a.Address == s && len(s) <= 254
}

var dateRe = regexp.MustCompile(`(?i)(?:\b(\d{4})-(\d{2})-(\d{2})|\b(\d{1,2})\.(\d{1,2})\.(\d{4}|\d{2})?)(?:,?\s*(?:T|um|at|@)?\s*(\d{1,2})[:.](\d{2})\s*(?:uhr|h)?(?:\s*(?:–|-|bis|to|until)\s*(\d{1,2})[:.](\d{2}))?)?`)

func parseOption(text string, loc *time.Location, now time.Time) Option {
	o := Option{Text: text}
	idx := dateRe.FindStringSubmatchIndex(text)
	if idx == nil || (idx[1] < len(text) && text[idx[1]] >= '0' && text[idx[1]] <= '9') {
		return o
	}
	g := func(i int) string {
		if idx[2*i] < 0 {
			return ""
		}
		return text[idx[2*i]:idx[2*i+1]]
	}
	n := func(i int) int { v, _ := strconv.Atoi(g(i)); return v }
	var y, mo, d int
	if g(1) != "" {
		y, mo, d = n(1), n(2), n(3)
	} else {
		d, mo, y = n(4), n(5), n(6)
		switch len(g(6)) {
		case 0:
			y = now.In(loc).Year()
			if time.Date(y, time.Month(mo), d, 23, 59, 0, 0, loc).Before(now) {
				y++
			}
		case 2:
			y += 2000
		}
	}
	day := time.Date(y, time.Month(mo), d, 0, 0, 0, 0, loc)
	if day.Day() != d || int(day.Month()) != mo {
		return o
	}
	if g(7) == "" {
		o.Start, o.End, o.AllDay = day, day.AddDate(0, 0, 1), true
		return o
	}
	if n(7) > 23 || n(8) > 59 {
		return Option{Text: text}
	}
	o.Start = time.Date(y, time.Month(mo), d, n(7), n(8), 0, 0, loc)
	o.End = o.Start.Add(time.Hour)
	if g(9) != "" && n(9) <= 23 && n(10) <= 59 {
		o.End = time.Date(y, time.Month(mo), d, n(9), n(10), 0, 0, loc)
		if !o.End.After(o.Start) {
			o.End = o.End.AddDate(0, 0, 1)
		}
	}
	return o
}

func parseOptions(text string, loc *time.Location, now time.Time) []Option {
	var opts []Option
	for line := range strings.Lines(text) {
		if line = strings.TrimSpace(line); line != "" {
			opts = append(opts, parseOption(line, loc, now))
		}
	}
	return opts
}

func icsEscape(s string) string {
	return strings.NewReplacer(`\`, `\\`, ";", `\;`, ",", `\,`, "\r\n", `\n`, "\n", `\n`, "\r", `\n`).Replace(s)
}

func icsFold(line string) string {
	var b strings.Builder
	n := 0
	for _, r := range line {
		l := len(string(r))
		if n+l > 75 {
			b.WriteString("\r\n ")
			n = 1
		}
		b.WriteRune(r)
		n += l
	}
	return b.String()
}

func buildICS(uid, title, desc string, o Option, now time.Time) []byte {
	const utc = "20060102T150405Z"
	lines := []string{
		"BEGIN:VCALENDAR", "VERSION:2.0", "PRODID:-//nobloat//poll//EN", "METHOD:PUBLISH",
		"BEGIN:VEVENT",
		"UID:" + uid,
		"DTSTAMP:" + now.UTC().Format(utc),
	}
	if o.AllDay {
		lines = append(lines, "DTSTART;VALUE=DATE:"+o.Start.Format("20060102"), "DTEND;VALUE=DATE:"+o.End.Format("20060102"))
	} else {
		lines = append(lines, "DTSTART:"+o.Start.UTC().Format(utc), "DTEND:"+o.End.UTC().Format(utc))
	}
	lines = append(lines, "SUMMARY:"+icsEscape(title))
	if desc != "" {
		lines = append(lines, "DESCRIPTION:"+icsEscape(desc))
	}
	lines = append(lines, "END:VEVENT", "END:VCALENDAR")
	var b strings.Builder
	for _, l := range lines {
		b.WriteString(icsFold(l) + "\r\n")
	}
	return []byte(b.String())
}

var messages = loadMessages()

func loadMessages() map[string]map[string]string {
	files, _ := assets.ReadDir("i18n")
	m := map[string]map[string]string{}
	for _, f := range files {
		b, _ := assets.ReadFile("i18n/" + f.Name())
		var kv map[string]string
		if err := json.Unmarshal(b, &kv); err != nil {
			panic(fmt.Sprintf("i18n/%s: %v", f.Name(), err))
		}
		m[strings.TrimSuffix(f.Name(), path.Ext(f.Name()))] = kv
	}
	return m
}

func T(lang, key string, args ...any) string {
	s, ok := messages[lang][key]
	if !ok {
		if s, ok = messages["en"][key]; !ok {
			return "!missing:" + key
		}
	}
	if len(args) > 0 {
		return fmt.Sprintf(s, args...)
	}
	return s
}

func detectLang(header string) string {
	for part := range strings.SplitSeq(header, ",") {
		tag, params, _ := strings.Cut(strings.TrimSpace(part), ";")
		base, _, _ := strings.Cut(strings.ToLower(tag), "-")
		if _, ok := messages[base]; ok && strings.TrimSpace(params) != "q=0" {
			return base
		}
	}
	return "en"
}

// singleLine reports whether s has no line breaks; titles and names go into mail subjects and greetings.
func singleLine(s string) bool { return !strings.ContainsAny(s, "\r\n") }

// limiter counts hits per key in fixed windows of length every.
// ponytail: fixed windows allow up to twice a limit across a window boundary; sliding windows if that matters.
type limiter struct {
	every time.Duration
	mu    sync.Mutex
	start time.Time
	n     map[string]int
}

type limit struct {
	key string
	max int
}

// allow counts one hit on every key if all of them are below their max (max 0 means no limit), else none.
func (l *limiter) allow(now time.Time, ls ...limit) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.n == nil || now.Sub(l.start) >= l.every {
		l.start, l.n = now, map[string]int{}
	}
	for _, x := range ls {
		if x.max > 0 && l.n[x.key] >= x.max {
			return false
		}
	}
	for _, x := range ls {
		l.n[x.key]++
	}
	return true
}

// over reports whether key reached max in the current window, without counting a hit.
func (l *limiter) over(now time.Time, key string, max int) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.n != nil && now.Sub(l.start) < l.every && l.n[key] >= max
}
