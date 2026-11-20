package main

// Regression tests for the review fixes, owner decision 2026-11-20 (POLL-1 to POLL-12).

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/textproto"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAnswerCap(t *testing.T) {
	e := newEnv(t)
	e.createPoll("dinner", opts, false, false)
	e.srv.store.update("dinner", func(p *Poll) (string, error) {
		for range maxAnswers {
			p.Answers = append(p.Answers, &Answer{Token: rand.Text(), Name: "P"})
		}
		return "test", nil
	})
	if r := e.post("/p/dinner", url.Values{"name": {"One too many"}, "choice": {"0"}}); r.status != http.StatusConflict || len(e.poll("dinner").Answers) != maxAnswers {
		t.Fatalf("answer beyond the cap: %d", r.status)
	}
}

func TestLineBreaksRejected(t *testing.T) {
	e := newEnv(t, func(c *config) { c.PublicCreate = true })
	e.createPoll("dinner", opts, false, false)
	for _, f := range []url.Values{
		{"title": {"Spam\r\nhttp://evil"}, "options": {"A"}, "name": {"N"}},
		{"title": {"T"}, "options": {"A"}, "name": {"N\n\nhttp://evil"}},
	} {
		if r := e.post("/new", f); r.status != http.StatusBadRequest {
			t.Errorf("line break in %v: %d", f, r.status)
		}
	}
	if r := e.post("/p/dinner", url.Values{"name": {"A\rB"}, "choice": {"0"}}); r.status != http.StatusBadRequest {
		t.Errorf("line break in name: %d", r.status)
	}
	if r := e.post(e.manage("dinner")+"/edit", url.Values{"title": {"A\nB"}}); r.status != http.StatusBadRequest || e.poll("dinner").Title != "Team Dinner Ü" {
		t.Errorf("line break in edited title: %d", r.status)
	}
}

func TestMailRateLimits(t *testing.T) {
	e := newEnv(t, func(c *config) { c.MailPerAddress, c.MailPerHour = 2, 3 })
	e.createPoll("dinner", opts, false, false)
	e.srv.mails = limiter{every: time.Hour} // the creation mail does not count
	for i := range 3 {
		e.submit("dinner", fmt.Sprint("P", i), fmt.Sprintf("p%d@example.com", i), 0)
	}
	if n := len(e.mailsTo("owner@example.com")); n != 1 || e.mailCount() != 3 {
		t.Fatalf("want 3 in total, got owner %d, total %d", n, e.mailCount())
	}
	e.mu.Lock()
	e.now = e.now.Add(time.Hour)
	e.mu.Unlock()
	e.submit("dinner", "Later", "later@example.com", 0)
	e.submit("dinner", "Later", "later@example.com", 0)
	e.submit("dinner", "Later", "later@example.com", 0)
	if n := len(e.mailsTo("later@example.com")); n != 2 {
		t.Fatalf("the limit must reset after an hour and allow 2 per address: %d", n)
	}
}

func TestCreationLimit(t *testing.T) {
	e := newEnv(t, func(c *config) { c.PublicCreate, c.CreatePerHour = true, 1 })
	f := url.Values{"title": {"T"}, "options": {"A"}, "name": {"N"}}
	if r := e.post("/new", f); r.status != http.StatusSeeOther {
		t.Fatalf("first: %d", r.status)
	}
	if r := e.post("/new", f); r.status != http.StatusTooManyRequests {
		t.Fatalf("second public creation in the hour: %d", r.status)
	}
	if r := e.adminPost("/new", f); r.status != http.StatusSeeOther {
		t.Fatalf("the admin is not limited: %d", r.status)
	}
}

func TestMailDifficulty(t *testing.T) {
	e := newEnv(t, func(c *config) { c.PublicCreate = true })
	e.createPoll("dinner", opts, false, false)
	e.srv.cfg.MailDifficulty = 6
	if r := e.get("/pow?mail=1"); !strings.HasPrefix(r.body, "6 ") {
		t.Fatalf("GET /pow?mail=1: %q", r.body)
	}
	if r := e.post("/p/dinner", url.Values{"name": {"A"}, "email": {"a@example.com"}, "choice": {"0"}}); r.status != http.StatusBadRequest || len(e.poll("dinner").Answers) != 0 {
		t.Fatalf("easy proof with an email: %d", r.status)
	}
	if r := e.post("/new", url.Values{"title": {"T"}, "options": {"A"}, "name": {"N"}, "email": {"a@example.com"}}); r.status != http.StatusBadRequest {
		t.Fatalf("easy proof creating with an email: %d", r.status)
	}
	if r := e.post("/p/dinner", url.Values{"name": {"B"}, "choice": {"0"}}); r.status != http.StatusSeeOther {
		t.Fatalf("easy proof without an email: %d", r.status)
	}
	ch := e.srv.pow.issue(6)
	if r := e.post("/p/dinner", url.Values{"name": {"A"}, "email": {"a@example.com"}, "choice": {"0"}, "pow": {ch}, "nonce": {solveForTest(ch, 6)}}); r.status != http.StatusSeeOther {
		t.Fatalf("hard proof with an email: %d", r.status)
	}
}

func TestPowReplaySetBounded(t *testing.T) {
	now := time.Now()
	p := newPow(0, func() time.Time { return now })
	for range 100 {
		p.verify(p.issue(0), "0")
	}
	now = now.Add(2 * powTTL)
	ch := p.issue(0)
	p.verify(ch, "0")
	now = now.Add(powTTL)
	p.verify(p.issue(0), "0")
	if len(p.used)+len(p.prev) != 2 {
		t.Fatalf("spent challenges not dropped: %d + %d", len(p.used), len(p.prev))
	}
	if p.verify(ch, "0") {
		t.Fatal("replay accepted")
	}
}

func TestChangedNotification(t *testing.T) {
	e := newEnv(t)
	e.createPoll("dinner", opts, true, false)
	l := e.answer("dinner", "Anna", "anna@example.com", 0)
	n := len(e.mailsTo("owner@example.com"))
	e.do("POST", l+"/update", url.Values{"choice": {"0"}}, false, "X-Autosave", "1")
	if len(e.mailsTo("owner@example.com")) != n {
		t.Fatal("unchanged choices must not mail the owner")
	}
	for _, c := range [][]string{{"0", "1"}, {"1"}, {"2"}} {
		e.do("POST", l+"/update", url.Values{"choice": c}, false, "X-Autosave", "1")
	}
	if got := len(e.mailsTo("owner@example.com")) - n; got != 1 {
		t.Fatalf("autosave clicks: want 1 changed mail, got %d", got)
	}
}

func TestSweep(t *testing.T) {
	e := newEnv(t)
	e.createPoll("old", opts, false, false)
	e.createPoll("fresh", opts, false, false)
	old := time.Now().AddDate(0, 0, -retentionDays-1)
	os.Chtimes(filepath.Join(e.dataDir, "polls", "old.json"), old, old)
	e.srv.store.sweep(time.Now().AddDate(0, 0, -retentionDays))
	if _, err := e.srv.store.get("old"); err != errNotFound {
		t.Fatal("inactive poll not deleted")
	}
	e.poll("fresh")
}

func TestListSkipsBadFiles(t *testing.T) {
	e := newEnv(t)
	e.createPoll("dinner", opts, false, false)
	os.WriteFile(filepath.Join(e.dataDir, "polls", "broken.json"), []byte("{"), 0o600)
	os.WriteFile(filepath.Join(e.dataDir, "polls", "Poll_copy.json"), []byte("{}"), 0o600)
	if r := e.adminGet("/a"); r.status != 200 || !strings.Contains(r.body, "Team Dinner") {
		t.Fatalf("admin list with a bad file: %d", r.status)
	}
}

func TestAdminAuthThrottle(t *testing.T) {
	e := newEnv(t)
	for range authFailures {
		req, _ := http.NewRequest("GET", e.ts.URL+"/a", nil)
		req.SetBasicAuth("admin", "wrong")
		req.Header.Set("X-Forwarded-For", "203.0.113.7")
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		res.Body.Close()
	}
	if r := e.do("GET", "/a", nil, true, "X-Forwarded-For", "203.0.113.7"); r.status != http.StatusTooManyRequests {
		t.Fatalf("after %d failures: %d", authFailures, r.status)
	}
	if r := e.do("GET", "/a", nil, true, "X-Forwarded-For", "198.51.100.1"); r.status != 200 {
		t.Fatalf("another client: %d", r.status)
	}
}

func TestAdminEmailDefault(t *testing.T) {
	e := newEnv(t, func(c *config) { c.AdminEmail, c.PublicCreate = "admin@example.com", true })
	f := url.Values{"title": {"T"}, "options": {"A"}, "name": {"N"}}
	if p := e.poll(createdID(e.adminPost("/new", f))); p.OwnerEmail != "admin@example.com" || len(e.mailsTo("admin@example.com")) != 1 {
		t.Fatalf("admin-created poll owner email %q", p.OwnerEmail)
	}
	if p := e.poll(createdID(e.post("/new", f))); p.OwnerEmail != "" {
		t.Fatalf("public poll got owner email %q", p.OwnerEmail)
	}
}

func TestICSEscapeBareCR(t *testing.T) {
	if got := icsEscape("Meeting\rATTENDEE:x"); strings.Contains(got, "\r") {
		t.Fatalf("bare CR left in %q", got)
	}
}

func TestOutboxGivesUp(t *testing.T) {
	dir := t.TempDir()
	q, err := openOutbox(dir, func(m Mail) error {
		if m.To == "bounce@example.com" {
			return &textproto.Error{Code: 550, Msg: "no such user"}
		}
		return errors.New("connection refused")
	})
	if err != nil {
		t.Fatal(err)
	}
	defer q.shutdown(context.Background())
	old, _ := json.Marshal(mailJob{Mail: Mail{To: "old@example.com", Subject: "s", Text: "t"}, Created: time.Now().Add(-outboxGiveUp)})
	oldPath := filepath.Join(dir, "mail", "mail-old.json")
	if err := writeOutboxFile(filepath.Join(dir, "mail"), oldPath, old); err != nil {
		t.Fatal(err)
	}
	q.mu.Lock()
	q.due[oldPath] = time.Time{}
	q.mu.Unlock()
	if err := q.enqueue(Mail{To: "bounce@example.com", Subject: "s", Text: "t"}); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(2 * time.Second); q.pending() && time.Now().Before(deadline); time.Sleep(time.Millisecond) {
	}
	if failed, _ := filepath.Glob(filepath.Join(dir, "mail", "failed", "mail-*.json")); q.pending() || len(failed) != 2 {
		t.Fatalf("a 5xx and a 3-day-old mail must move to failed/: %v", failed)
	}
}

func TestOutboxSkipsInvalidJobAtStart(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "mail"), 0o700)
	os.WriteFile(filepath.Join(dir, "mail", "mail-bad.json"), []byte("{"), 0o600)
	q, err := openOutbox(dir, func(Mail) error { return nil })
	if err != nil {
		t.Fatal(err)
	}
	defer q.shutdown(context.Background())
	if _, err := os.Stat(filepath.Join(dir, "mail", "mail-bad.json.invalid")); err != nil || q.pending() {
		t.Fatal("an unreadable job must be set aside, not block the outbox")
	}
}
