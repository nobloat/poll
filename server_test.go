package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"maps"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/mail"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

type testEnv struct {
	t       *testing.T
	srv     *server
	ts      *httptest.Server
	dataDir string
	mu      sync.Mutex
	now     time.Time
	mails   []Mail
}

func newEnv(t *testing.T, opts ...func(*config)) *testEnv {
	t.Helper()
	loc, _ := time.LoadLocation("Europe/Vienna")
	e := &testEnv{t: t, now: time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC), dataDir: t.TempDir()}
	cfg := config{DataDir: e.dataDir, BaseURL: "https://poll.example", AdminPassword: "secret", Loc: loc}
	for _, o := range opts {
		o(&cfg)
	}
	s, err := newServer(cfg, e.clock, e.record)
	if err != nil {
		t.Fatal(err)
	}
	e.srv = s
	e.ts = httptest.NewServer(s.handler())
	t.Cleanup(e.ts.Close)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := s.outbox.shutdown(ctx); err != nil {
			t.Error(err)
		}
	})
	return e
}

func (e *testEnv) clock() time.Time {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.now
}

func (e *testEnv) record(m Mail) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.mails = append(e.mails, m)
	return nil
}

func (e *testEnv) mailCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return len(e.mails)
}

func (e *testEnv) waitOutbox() {
	e.t.Helper()
	for deadline := time.Now().Add(5 * time.Second); e.srv.outbox.pending(); time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			e.t.Fatal("mail outbox did not drain")
		}
	}
}

func (e *testEnv) mailsTo(addr string) []Mail {
	e.mu.Lock()
	defer e.mu.Unlock()
	var ms []Mail
	for _, m := range e.mails {
		if m.To == addr {
			ms = append(ms, m)
		}
	}
	return ms
}

var linkRe = regexp.MustCompile(`/p/[a-z0-9-]+/a/[A-Z2-7]+`)

func (e *testEnv) linkPath(addr string) string {
	e.t.Helper()
	ms := e.mailsTo(addr)
	if len(ms) == 0 {
		e.t.Fatalf("no mail to %s", addr)
	}
	l := linkRe.FindString(ms[len(ms)-1].Text)
	if l == "" {
		e.t.Fatalf("no link in mail: %s", ms[len(ms)-1].Text)
	}
	return l
}

type resp struct {
	status int
	body   string
	header http.Header
}

func (e *testEnv) do(method, path string, form url.Values, admin bool, hdr ...string) resp {
	e.t.Helper()
	var body io.Reader
	if method == "POST" {
		form = maps.Clone(form)
		if form == nil {
			form = url.Values{}
		}
		if !form.Has("pow") {
			form.Set("pow", e.srv.pow.issue(0))
			form.Set("nonce", "0")
		}
		body = strings.NewReader(form.Encode())
	}
	req, _ := http.NewRequest(method, e.ts.URL+path, body)
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if admin {
		req.SetBasicAuth("admin", "secret")
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	c := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := c.Do(req)
	if err != nil {
		e.t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	e.waitOutbox()
	return resp{res.StatusCode, string(b), res.Header}
}

func (e *testEnv) get(path string) resp                     { return e.do("GET", path, nil, false) }
func (e *testEnv) post(path string, f url.Values) resp      { return e.do("POST", path, f, false) }
func (e *testEnv) adminGet(path string) resp                { return e.do("GET", path, nil, true) }
func (e *testEnv) adminPost(path string, f url.Values) resp { return e.do("POST", path, f, true) }

func (e *testEnv) createPoll(id, options string, multiple, visible bool) {
	e.t.Helper()
	f := url.Values{"title": {"Team Dinner Ü"}, "description": {"Let's eat"}, "options": {options}, "name": {"Owner"}, "email": {"owner@example.com"}}
	if !multiple {
		f.Set("mode", "single")
	}
	if visible {
		f.Set("results", "1")
	}
	r := e.adminPost("/new", f)
	if r.status != http.StatusSeeOther {
		e.t.Fatalf("create poll: %d %s", r.status, r.body)
	}
	e.mu.Lock()
	e.mails = nil
	e.mu.Unlock()
	e.rename(createdID(r), id)
}

func (e *testEnv) manage(id string) string { return "/p/" + id + "/m/" + e.poll(id).OwnerToken }

func createdID(r resp) string { return strings.Split(r.header.Get("Location"), "/")[2] }

func (e *testEnv) rename(from, to string) {
	e.t.Helper()
	p := e.poll(from)
	p.ID = to
	if err := e.srv.store.create(p, "test"); err != nil {
		e.t.Fatal(err)
	}
	if err := e.srv.store.delete(from); err != nil {
		e.t.Fatal(err)
	}
}

func (e *testEnv) submit(id, name, email string, choices ...int) resp {
	e.t.Helper()
	f := url.Values{"name": {name}, "email": {email}}
	for _, c := range choices {
		f.Add("choice", strconv.Itoa(c))
	}
	return e.follow(e.post("/p/"+id, f))
}

func (e *testEnv) follow(r resp) resp {
	if r.status == http.StatusSeeOther {
		return e.get(r.header.Get("Location"))
	}
	return r
}

func (e *testEnv) answer(id, name, email string, choices ...int) string {
	e.t.Helper()
	if r := e.submit(id, name, email, choices...); r.status != 200 {
		e.t.Fatalf("submit: %d %s", r.status, r.body)
	}
	return e.linkPath(email)
}

func (e *testEnv) poll(id string) *Poll {
	e.t.Helper()
	p, err := e.srv.store.get(id)
	if err != nil {
		e.t.Fatal(err)
	}
	return p
}

const opts = "Mo, 12.10.2026, 14:00–15:30\nDi, 13.10.2026\nPizza"

func TestResultsVisibility(t *testing.T) {
	for _, visible := range []bool{false, true} {
		e := newEnv(t)
		e.createPoll("dinner", opts, false, visible)
		la := e.answer("dinner", "Anna", "anna@example.com", 0)
		e.answer("dinner", "Bertram", "bertram@example.com", 1)

		if r := e.get("/p/dinner"); strings.Contains(r.body, "Bertram") != visible || strings.Contains(r.body, "@example.com") {
			t.Fatalf("public page: visible=%v but responses shown=%v (emails never)", visible, !visible)
		}
		check := func() {
			r := e.get(la)
			if !strings.Contains(r.body, "Anna") {
				t.Fatal("participant page misses own response")
			}
			if strings.Contains(r.body, "Bertram") != visible {
				t.Fatalf("visible=%v but other response shown=%v", visible, !visible)
			}
			if strings.Contains(r.body, "@example.com") {
				t.Fatal("participant page shows emails")
			}
		}
		check()
		e.post(e.manage("dinner")+"/close", url.Values{"option": {"1"}})
		check()
		if r := e.get(e.manage("dinner")); !strings.Contains(r.body, "Bertram") || strings.Contains(r.body, "bertram@example.com") {
			t.Fatal("the manage page shows every answer, but never an email address")
		}
	}
}

func TestSecurity(t *testing.T) {
	e := newEnv(t)
	e.createPoll("dinner", opts, false, false)
	r := e.do("POST", "/new", url.Values{"title": {"T"}, "options": {"A"}}, true, "Sec-Fetch-Site", "cross-site")
	if r.status != 403 {
		t.Fatalf("cross-site POST: %d", r.status)
	}
	if r := e.do("POST", "/new", url.Values{"title": {"T"}, "options": {"A"}}, true, "Origin", "https://evil.example"); r.status != 403 {
		t.Fatalf("cross-origin POST: %d", r.status)
	}
	if _, err := e.srv.store.get("evil"); err != errNotFound {
		t.Fatal("cross-origin request created a poll")
	}
	for _, p := range []string{"/p/dinner", "/a", "/nope", "/static/app.css", "/theme.css"} {
		h := e.get(p).header
		if !strings.Contains(h.Get("Content-Security-Policy"), "default-src 'self'") || !strings.Contains(h.Get("Content-Security-Policy"), "frame-ancestors 'none'") ||
			h.Get("X-Content-Type-Options") != "nosniff" || h.Get("Referrer-Policy") != "no-referrer" {
			t.Fatalf("%s: missing security headers: %v", p, h)
		}
	}
	big := url.Values{"name": {strings.Repeat("x", 100<<10)}}
	if r := e.post("/p/dinner", big); r.status != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized body: %d", r.status)
	}
	if r := e.submit("dinner", strings.Repeat("x", 101), "a@example.com", 0); r.status != 400 {
		t.Fatalf("long name: %d", r.status)
	}
}

func TestPathTraversal(t *testing.T) {
	e := newEnv(t)
	os.WriteFile(filepath.Join(e.dataDir, "secret.json"), []byte(`{"title": "SECRET"}`), 0o600)
	for _, path := range []string{"/p/..%2Fsecret", "/p/%2e%2e%2fsecret", "/p/..%5Csecret", "/p/../secret", "/p/..%2Fsecret/m/x", "/static/..%2F..%2Fmain.go", "/static/../main.go", "/theme.css?theme=../../main"} {
		if r := e.get(path); strings.Contains(r.body, "SECRET") || strings.Contains(r.body, "package main") {
			t.Errorf("%s leaked a file outside its directory", path)
		}
	}
	for _, id := range []string{"../secret", "..", "a/b", "A", ""} {
		if validID(id) {
			t.Errorf("validID(%q) must be false", id)
		}
	}
}

func TestPowSubmit(t *testing.T) {
	e := newEnv(t)
	e.createPoll("dinner", opts, false, false)
	e.srv.pow.difficulty = 8
	ch := e.srv.pow.issue(0)
	nonce := solveForTest(ch, 8)
	f := url.Values{"name": {"A"}, "email": {"a@example.com"}, "choice": {"0"}, "pow": {ch}, "nonce": {nonce}}
	if r := e.post("/p/dinner", f); r.status != 303 {
		t.Fatalf("valid pow: %d", r.status)
	}
	f.Set("email", "b@example.com")
	if r := e.post("/p/dinner", f); r.status != 400 || !strings.Contains(r.body, T("en", "err.pow")) {
		t.Fatalf("replayed pow: %d", r.status)
	}
}

func TestMailFormat(t *testing.T) {
	e := newEnv(t)
	e.createPoll("dinner", opts, false, false)
	e.srv.store.update("dinner", func(p *Poll) (string, error) {
		p.OwnerToken, p.OwnerEmail, p.OwnerLang = "OWNER", "owner@example.com", "en"
		return "test", nil
	})
	e.do("POST", "/p/dinner", url.Values{"name": {"Jörg"}, "email": {"j@example.com"}, "choice": {"0"}}, false, "Accept-Language", "de-AT,de;q=0.9,en;q=0.5")
	m := e.mailsTo("j@example.com")[0]
	if m.Subject != T("de", "mail.link.subject", "Team Dinner Ü") || !strings.Contains(m.Text, "Hallo Jörg") {
		t.Fatalf("participant language not used: %q", m.Subject)
	}
	if a := e.mailsTo("owner@example.com"); !strings.HasPrefix(a[0].Subject, "New answer") {
		t.Fatalf("creator mail not in the creator's language: %q", a[0].Subject)
	}
	if a := e.mailsTo("owner@example.com")[0]; strings.Contains(a.Text, "Pizza") || !strings.Contains(a.Text, "  x Mo, 12.10.2026, 14:00–15:30") || !strings.Contains(a.Text, "Jörg") || !strings.Contains(a.Text, "/m/OWNER") || strings.Contains(a.Text, "j@example.com") {
		t.Fatalf("creator mail must carry the choices and the link only (POLL-1), never the email:\n%s", a.Text)
	}
	e.post(e.manage("dinner")+"/close", url.Values{"option": {"0"}})
	ms := e.mailsTo("j@example.com")
	closed := ms[len(ms)-1]
	raw, err := buildMessage("poll@example.com", closed, e.clock())
	if err != nil {
		t.Fatal(err)
	}
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	subj, err := new(mime.WordDecoder).DecodeHeader(msg.Header.Get("Subject"))
	if err != nil || subj != "Ergebnis: Team Dinner Ü" {
		t.Fatalf("subject round trip: %q %v", subj, err)
	}
	mt, params, _ := mime.ParseMediaType(msg.Header.Get("Content-Type"))
	if mt != "multipart/mixed" {
		t.Fatalf("top: %s", mt)
	}
	mr := multipart.NewReader(msg.Body, params["boundary"])
	text, _ := mr.NextPart()
	body, _ := io.ReadAll(text)
	if pt, _, _ := mime.ParseMediaType(text.Header.Get("Content-Type")); pt != "text/plain" || !strings.Contains(string(body), "Jörg") {
		t.Fatalf("text part: %s %s", pt, body)
	}
	ics, err := mr.NextPart()
	if err != nil || !strings.HasPrefix(ics.Header.Get("Content-Type"), "text/calendar") {
		t.Fatalf("ics part: %v", err)
	}
}

func TestTemplatesRender(t *testing.T) {
	e := newEnv(t)
	o := parseOption("Mo, 12.10.2026, 14:00–15:30", time.UTC, time.Now())
	a := &Answer{Token: "TOKEN", Name: "Anna", Email: "a@example.com", Choices: []int{0}}
	p := &Poll{ID: "s", Title: "T", Description: "D", Options: []Option{o, {Text: "B"}}, Answers: []*Answer{a, {Name: "P"}}, Closed: true, ResultsVisible: true}
	for lang := range messages {
		for name, tpl := range e.srv.pages {
			for _, closed := range []bool{false, true} {
				for _, multiple := range []bool{false, true} {
					p.Closed, p.Multiple = closed, multiple
					pg := &page{Lang: lang, Title: "T", Poll: p, Polls: []*Poll{p}, Answer: a, Results: results(p),
						Error: "err.name", Notice: "saved", Form: url.Values{"choice": {"0"}}, Text: "remove.text", Action: "/x", Button: "remove.button", Base: "/p/s/m/T", Public: true, Link: "/y", LinkText: "sent.link"}
					var buf bytes.Buffer
					if err := tpl.ExecuteTemplate(&buf, "layout", pg); err != nil {
						t.Fatalf("%s/%s: %v", lang, name, err)
					}
					if strings.Contains(buf.String(), "!missing:") {
						t.Fatalf("%s/%s: missing translation: %s", lang, name, regexp.MustCompile(`!missing:\S+`).FindString(buf.String()))
					}
				}
			}
		}
		for _, name := range []string{"link", "admin", "closed", "manage"} {
			for _, ev := range []string{"new", "changed", "removed"} {
				d := mailData{Lang: lang, Poll: p, Answer: a, Public: "https://x/p/s", Link: "https://x", Event: ev, Choices: []string{"A"}}
				var buf bytes.Buffer
				if err := e.srv.mailText.ExecuteTemplate(&buf, name+".txt", d); err != nil {
					t.Fatal(err)
				}
				if strings.Contains(buf.String(), "!missing:") {
					t.Fatalf("%s/%s mail: missing translation", lang, name)
				}
			}
		}
	}
}

func TestI18nKeysMatch(t *testing.T) {
	for lang, m := range messages {
		for k := range messages["en"] {
			if _, ok := m[k]; !ok {
				t.Errorf("%s misses %q", lang, k)
			}
		}
		for k := range m {
			if _, ok := messages["en"][k]; !ok {
				t.Errorf("en misses %q (from %s)", k, lang)
			}
		}
	}
	if len(messages) < 2 {
		t.Fatal("expected at least en and de")
	}
}

func TestSeedPolls(t *testing.T) {
	e := newEnv(t)
	files, _ := filepath.Glob("seed/*.json")
	for _, f := range files {
		b, _ := os.ReadFile(f)
		os.WriteFile(filepath.Join(e.dataDir, "polls", filepath.Base(f)), b, 0o600)
	}
	polls, err := e.srv.store.list()
	if err != nil || len(polls) != 4 {
		t.Fatalf("seed: %v %d", err, len(polls))
	}
	for _, p := range polls {
		if p.Closed || len(p.Answers) == 0 || p.Theme == "" || p.OwnerToken == "" {
			t.Fatalf("%s: seed polls must be open, answered, themed and have a creator", p.ID)
		}
	}
	if r := e.get(e.manage("pizza-friday")); r.status != 200 || !strings.Contains(r.body, "Rubber Duck") {
		t.Fatal("seed poll not rendered")
	}
	if r := e.get("/p/pizza-friday/a/" + e.poll("pizza-friday").Answers[0].Token); r.status != 200 || !strings.Contains(r.body, "Grace Hopper") {
		t.Fatal("seed participant page")
	}
}

func TestResultsSortedByCount(t *testing.T) {
	p := &Poll{Options: []Option{{Text: "A"}, {Text: "B"}, {Text: "C"}}, Answers: []*Answer{
		{Name: "x", Choices: []int{1, 2}}, {Name: "y", Choices: []int{2}}, {Name: "z", Choices: []int{1}},
	}}
	rs := results(p)
	var got []string
	for _, r := range rs {
		got = append(got, fmt.Sprintf("%s%d%v", r.Option.Text, len(r.Names), r.Lead))
	}
	if strings.Join(got, " ") != "B2true C2true A0false" {
		t.Fatalf("got %v", got)
	}
}

func TestPublicCreation(t *testing.T) {
	off := newEnv(t)
	if r := off.get("/new"); r.status != 401 || off.adminGet("/new").status != 200 {
		t.Fatalf("/new without PUBLIC_CREATE must need the admin login: %d", r.status)
	}
	e := newEnv(t, func(c *config) { c.PublicCreate = true })
	if r := e.get("/p/lunch"); !strings.Contains(r.body, `class="home logo" href="/"`) {
		t.Fatal("with a start page, the logo links to it")
	}
	if r := e.get("/"); r.status != 200 || !strings.Contains(r.body, `href="/new"`) || !strings.Contains(r.body, T("en", "home.pick.title")) {
		t.Fatal("root should be the start page explaining nobloat/poll, linking to /new")
	}
	if r := off.get("/"); r.header.Get("Location") != "/a" {
		t.Fatal("without PUBLIC_CREATE the root leads to /a")
	}
	if r := e.get("/new"); r.status != 200 || !strings.Contains(r.body, `class="pow-status"`) || strings.Contains(r.body, `href="/a"`) {
		t.Fatalf("public form: %d", r.status)
	}
	f := url.Values{"title": {"Lunch"}, "options": {"Pho\nRamen"}, "name": {"Olga"}, "email": {"Olga@Example.org"}, "pow": {""}}
	if r := e.post("/new", f); r.status != 400 || !strings.Contains(r.body, T("en", "err.pow")) {
		t.Fatalf("missing PoW: %d", r.status)
	}
	f.Del("pow")
	r := e.post("/new", f)
	if r.status != 303 {
		t.Fatalf("create: %d %s", r.status, r.body)
	}
	id := createdID(r)
	if !validID(id) || len(id) != 16 {
		t.Fatalf("poll ID %q", id)
	}
	e.rename(id, "lunch")
	p := e.poll("lunch")
	if p.OwnerName != "Olga" || p.OwnerEmail != "olga@example.org" || r.header.Get("Location") != "/p/"+id+"/m/"+p.OwnerToken+"/created" {
		t.Fatalf("create: %d %s", r.status, r.header.Get("Location"))
	}
	manage := "/p/lunch/m/" + p.OwnerToken
	if r := e.get(manage + "/created"); r.status != 200 || !strings.Contains(r.body, `href="`+manage+`"`) || !strings.Contains(r.body, T("en", "created.link")) {
		t.Fatal("created page must link to the manage page")
	}
	if ms := e.mailsTo("olga@example.org"); len(ms) != 1 || !strings.Contains(ms[0].Text, "https://poll.example/p/"+id+"/m/"+p.OwnerToken) || !strings.Contains(ms[0].Text, "https://poll.example/p/"+id+"\n") {
		t.Fatal("creator didn't get the manage link")
	}

	if r := e.get(manage); r.status != 200 || !strings.Contains(r.body, manage+"/edit") || strings.Contains(r.body, "/a/p/") {
		t.Fatalf("manage page: %d", r.status)
	}
	for _, bad := range []string{"/p/lunch/m/WRONG", "/p/lunch/m/WRONG/edit"} {
		if r := e.get(bad); r.status != 404 && r.status != 405 {
			t.Fatalf("%s: %d", bad, r.status)
		}
	}
	e.createPoll("other", opts, false, false)
	if r := e.get("/p/other/m/" + p.OwnerToken); r.status != 404 {
		t.Fatal("a creator token must only work on its own poll")
	}
	if r := e.post(manage+"/edit", url.Values{"title": {"Lunch!"}, "options": {"Pho\nRamen\nBánh mì"}}); r.status != 303 || e.poll("lunch").Title != "Lunch!" || len(e.poll("lunch").Options) != 3 {
		t.Fatalf("creator edit: %d", r.status)
	}

	admin, olga := len(e.mailsTo("admin@example.com")), len(e.mailsTo("olga@example.org"))
	e.answer("lunch", "Anna", "anna@example.com", 2)
	if len(e.mailsTo("admin@example.com")) != admin || len(e.mailsTo("olga@example.org")) != olga+1 {
		t.Fatal("vote notification must go to the creator")
	}
	if ms := e.mailsTo("olga@example.org"); strings.Contains(ms[len(ms)-1].Text, "anna@example.com") || strings.Contains(e.get(manage).body, "anna@example.com") {
		t.Fatal("participant emails are never shown to the creator")
	}

	if r := e.adminGet("/a"); !strings.Contains(r.body, "olga@example.org") || !strings.Contains(r.body, `href="`+manage+`"`) {
		t.Fatal("admin overview lists the creator and links to the manage page")
	}

	if r := e.post(manage+"/close", url.Values{"option": {"2"}}); r.status != 303 || !e.poll("lunch").Closed {
		t.Fatalf("creator close: %d", r.status)
	}
	if r := e.post(manage+"/delete", url.Values{}); r.status != 303 {
		t.Fatalf("creator delete: %d", r.status)
	}
	if r := e.get(manage); r.status != 404 {
		t.Fatal("manage link survives deletion")
	}
}

func TestStorage(t *testing.T) {

	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "polls"), 0o700)
	tmp := filepath.Join(dir, "polls", ".tmp-123")
	os.WriteFile(tmp, []byte("x"), 0o600)
	if _, err := openStore(dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(tmp); !os.IsNotExist(err) {
		t.Fatal("tmp file not removed")
	}

	e := newEnv(t)
	e.createPoll("dinner", opts, false, false)
	path := filepath.Join(e.dataDir, "polls", "dinner.json")

	fi, _ := os.Stat(path)
	di, _ := os.Stat(filepath.Dir(path))
	var p Poll
	b, _ := os.ReadFile(path)
	if fi.Mode().Perm() != 0o600 || di.Mode().Perm() != 0o700 || json.Unmarshal(b, &p) != nil || p.Options[0].Start.IsZero() || !p.Options[1].AllDay {
		t.Fatalf("perms %v %v or dates %+v", fi.Mode().Perm(), di.Mode().Perm(), p.Options)
	}

	var wg sync.WaitGroup
	for i := range 30 {
		wg.Go(func() { e.submit("dinner", "P", fmt.Sprintf("p%d@example.com", i), i%3) })
	}
	wg.Wait()
	if n := len(e.poll("dinner").Answers); n != 30 {
		t.Fatalf("want 30 answers, got %d", n)
	}
}

func TestAnswering(t *testing.T) {
	e := newEnv(t)
	e.createPoll("dinner", opts, true, false)
	e.createPoll("other", opts, false, false)
	r := e.submit("dinner", "Anna", " Anna@Example.com ", 1)
	l := e.linkPath("anna@example.com")
	if strings.Contains(r.body, "/a/") || !strings.Contains(r.body, html.EscapeString(T("en", "sent.mail"))) || len(e.mailsTo("anna@example.com")) != 1 || len(e.mailsTo("owner@example.com")) != 1 {
		t.Fatalf("submit: %d", r.status)
	}
	if c := results(e.poll("dinner")); c[0].Index != 1 || len(c[0].Names) != 1 {
		t.Fatal("answer must count without any confirmation")
	}

	// POLL-10: a duplicate looks exactly like a new answer; the address gets its existing link again.
	r2 := e.post("/p/dinner", url.Values{"name": {"Mallory"}, "email": {"anna@example.com"}, "choice": {"2"}})
	if p := e.poll("dinner"); r2.header.Get("Location") != "/p/dinner/sent" || len(p.Answers) != 1 || p.Answers[0].Name != "Anna" || len(e.mailsTo("owner@example.com")) != 1 {
		t.Fatal("duplicate submission changed something or answered differently")
	}
	if ms := e.mailsTo("anna@example.com"); len(ms) != 2 || !strings.Contains(ms[1].Text, l) {
		t.Fatal("duplicate submission must mail the existing link to the address")
	}

	token := l[strings.LastIndex(l, "/")+1:]
	if e.get(l).status != 200 || e.get("/p/other/a/"+token).status != 404 || !strings.Contains(e.get(e.manage("dinner")).body, l) {
		t.Fatal("participant link")
	}

	if r := e.post(l+"/update", url.Values{"choice": {"0", "2"}}); r.status != 303 || !strings.Contains(e.follow(r).body, T("en", "notice.saved")) {
		t.Fatalf("update: %d", r.status)
	}
	if c := e.poll("dinner").Answers[0].Choices; len(c) != 2 || c[0] != 0 || c[1] != 2 {
		t.Fatalf("choices: %v", c)
	}
	if e.post(l+"/update", url.Values{"choice": {"7"}}).status != 400 {
		t.Fatal("invalid choice accepted")
	}

	if r := e.do("POST", l+"/update", url.Values{"choice": {"1"}}, false, "X-Autosave", "1"); r.status != 204 || e.poll("dinner").Answers[0].Choices[0] != 1 {
		t.Fatalf("autosave: %d", r.status)
	}
	if r := e.do("POST", l+"/update", url.Values{"choice": {"9"}}, false, "X-Autosave", "1"); r.status != 400 || !strings.Contains(r.body, T("en", "err.choice")) || strings.Contains(r.body, "<html") {
		t.Fatalf("autosave error: %d %q", r.status, r.body)
	}
	if r := e.post(l+"/update", url.Values{}); r.status != 303 || len(e.poll("dinner").Answers[0].Choices) != 0 || !strings.Contains(e.get(e.manage("dinner")).body, T("en", "results.none")) {
		t.Fatalf("no choice: %d", r.status)
	}

	if e.get(l+"/remove").status != 200 || len(e.poll("dinner").Answers) != 1 {
		t.Fatal("GET remove must only show a confirmation")
	}
	if r := e.post(l+"/remove", url.Values{}); r.header.Get("Location") != "/p/dinner/removed" || !strings.Contains(e.follow(r).body, T("en", "removed.text")) {
		t.Fatalf("remove: %d", r.status)
	}
	if e.get(l).status != 404 {
		t.Fatal("removed link still works")
	}

	l2 := e.answer("dinner", "Ben", "ben@example.com", 1)
	e.post(e.manage("dinner")+"/remove", url.Values{"token": {e.poll("dinner").Answers[0].Token}})
	if e.get(l2).status != 404 {
		t.Fatal("admin-removed link still works")
	}
}

func TestPowEverywhere(t *testing.T) {
	e := newEnv(t, func(c *config) { c.PublicCreate = true })
	e.createPoll("dinner", opts, true, false)
	link := e.answer("dinner", "Anna", "anna@example.com", 0)
	m := e.manage("dinner")
	n := e.mailCount()
	none := url.Values{"pow": {""}, "title": {"X"}, "options": {"A"}, "name": {"N"}, "choice": {"1"}, "option": {"0"}, "token": {e.poll("dinner").Answers[0].Token}}
	for _, path := range []string{"/new", "/p/dinner", link + "/update", link + "/remove", m + "/edit", m + "/close", m + "/reopen", m + "/remove", m + "/delete"} {
		if r := e.post(path, none); r.status != http.StatusBadRequest || !strings.Contains(r.body, T("en", "err.pow")) {
			t.Errorf("POST %s without proof of work: %d", path, r.status)
		}
	}
	if p := e.poll("dinner"); e.mailCount() != n || len(p.Answers) != 1 || !p.Answers[0].Chose(0) || p.Closed || p.Title != "Team Dinner Ü" {
		t.Fatal("requests without proof of work must change nothing")
	}
	if r := e.get("/pow"); r.status != 200 || !strings.HasPrefix(r.body, "0 ") {
		t.Fatalf("GET /pow must hand out difficulty and challenge: %q", r.body)
	}
}

func TestPageTitle(t *testing.T) {
	e := newEnv(t, func(c *config) { c.PublicCreate = true })
	e.createPoll("dinner", opts, true, false)
	for path, want := range map[string]string{"/": "<title>][✓</title>", "/new": "<title>][✓</title>", "/p/dinner": "<title>][✓ · Team Dinner Ü</title>", e.manage("dinner"): "<title>][✓ · Team Dinner Ü</title>"} {
		if !strings.Contains(e.get(path).body, want) {
			t.Errorf("%s: want %s", path, want)
		}
	}
}

func TestSubmitRevalidatesUnderLock(t *testing.T) {
	e := newEnv(t)
	e.createPoll("dinner", opts, false, false)
	edit := false
	e.srv.pow = newPow(0, func() time.Time {
		if edit {
			edit = false
			e.srv.store.update("dinner", func(p *Poll) (string, error) { p.Options = p.Options[:1]; return "test", nil })
		}
		return e.clock()
	})
	f := url.Values{"name": {"Anna"}, "choice": {"2"}}
	edit = true
	if r := e.post("/p/dinner", f); r.status != http.StatusBadRequest || len(e.poll("dinner").Answers) != 0 {
		t.Fatalf("a choice that vanished while submitting must be rejected: %d", r.status)
	}
	if e.get("/p/dinner").status != 200 {
		t.Fatal("poll page must still render")
	}
}

func TestAnsweringWithoutEmail(t *testing.T) {
	e := newEnv(t)
	e.createPoll("dinner", opts, true, false)
	r := e.submit("dinner", "Anna", "", 1)
	e.submit("dinner", "Ben", "  ", 2)
	if p := e.poll("dinner"); len(p.Answers) != 2 || !strings.Contains(r.body, html.EscapeString(T("en", "sent.text.nomail"))) || !strings.Contains(r.body, "/p/dinner/a/"+p.Answers[0].Token) {
		t.Fatal("answers without email must count, never as duplicates, and show the personal link")
	}
	if e.mailCount() != 2 || len(e.mailsTo("owner@example.com")) != 2 {
		t.Fatal("only the creator gets mail (one notification per answer)")
	}
	e.post(e.manage("dinner")+"/close", url.Values{"option": {"1"}})
	if !e.poll("dinner").Closed || e.mailCount() != 2 {
		t.Fatal("closing must not mail participants without email")
	}
}

func TestAnsweringWithNoOption(t *testing.T) {
	e := newEnv(t)
	e.createPoll("dinner", opts, false, true)
	e.submit("dinner", "Anna", "", 0)
	e.submit("dinner", "Lukas", "")
	if p := e.poll("dinner"); len(p.Answers) != 2 || len(p.Answers[1].Choices) != 0 {
		t.Fatal("an answer without any option must be accepted")
	}
	none := T("en", "results.none")
	if r := e.get("/p/dinner"); !strings.Contains(r.body, none+": "+`<span class="names">Lukas</span>`) {
		t.Fatal("public results must list who picked nothing separately")
	}
	if ms := e.mailsTo("owner@example.com"); !strings.Contains(ms[len(ms)-1].Text, "  "+T("en", "results.none")+"\n") {
		t.Fatalf("creator mail must list who picked nothing:\n%s", ms[len(ms)-1].Text)
	}
	if e.post("/p/dinner", url.Values{"name": {"Two"}, "choice": {"0", "1"}}).status != 400 {
		t.Fatal("single choice poll must still reject two options")
	}
}

func TestLimitsCountCharacters(t *testing.T) {
	e := newEnv(t)
	e.createPoll("dinner", opts, false, true)
	if r := e.post("/p/dinner", url.Values{"name": {strings.Repeat("ä", 100)}, "choice": {"0"}}); r.status != 303 {
		t.Fatalf("100 umlauts are 100 characters: %d", r.status)
	}
	if r := e.post("/p/dinner", url.Values{"name": {strings.Repeat("ä", 101)}, "choice": {"0"}}); r.status != 400 {
		t.Fatalf("101 characters must be rejected: %d", r.status)
	}
	ok := &Poll{Title: strings.Repeat("ü", 200), Options: []Option{{Text: strings.Repeat("ö", 200)}}}
	if k := validatePoll(ok); k != "" {
		t.Fatalf("200 characters must fit: %s", k)
	}
	ok.Options[0].Text += "ö"
	if validatePoll(ok) != "err.options" {
		t.Fatal("201 characters per option must be rejected")
	}
}

func TestCreatingWithoutEmail(t *testing.T) {
	e := newEnv(t)
	f := url.Values{"title": {"T"}, "options": {opts}, "name": {"Owner"}}
	r := e.adminPost("/new", f)
	if r.status != http.StatusSeeOther || e.mailCount() != 0 {
		t.Fatalf("creating without email must work and send nothing: %d", r.status)
	}
	p := e.poll(createdID(r))
	if b := e.follow(r).body; !strings.Contains(b, html.EscapeString(T("en", "created.text.nomail"))) || !strings.Contains(b, "/p/"+p.ID+"/m/"+p.OwnerToken) {
		t.Fatal("the created page must explain that only the shown link manages the poll")
	}
	e.submit(p.ID, "Anna", "", 0)
	if e.mailCount() != 0 {
		t.Fatal("no notifications without an owner email")
	}
	f.Set("email", "nope")
	f.Del("pow")
	if e.adminPost("/new", f).status != http.StatusBadRequest {
		t.Fatal("an invalid email must still be rejected")
	}
}

func TestClose(t *testing.T) {
	e := newEnv(t)
	e.createPoll("dinner", opts+"\nParty am 24.10. um 19.30 Uhr", true, false)

	e.srv.store.update("dinner", func(p *Poll) (string, error) { p.Options[3] = Option{Text: p.Options[3].Text}; return "test", nil })
	l := e.answer("dinner", "Anna", "anna@example.com", 0, 3)
	e.answer("dinner", "Ben", "ben@example.com", 2)
	before := e.mailCount()
	if e.get(e.manage("dinner")+"/close?option=0").status != 200 || e.poll("dinner").Closed || e.mailCount() != before {
		t.Fatal("GET close must only show a confirmation")
	}
	e.post(e.manage("dinner")+"/close", url.Values{"option": {"0"}})

	for _, addr := range []string{"anna@example.com", "ben@example.com"} {
		ms := e.mailsTo(addr)
		if m := ms[len(ms)-1]; !bytes.Contains(m.ICS, []byte("DTSTART:20261012T120000Z")) {
			t.Fatalf("missing/wrong ICS: %s", m.ICS)
		}
	}
	if e.mailCount() != before+2 {
		t.Fatalf("want 2 close mails, got %d", e.mailCount()-before)
	}

	if e.submit("dinner", "Late", "late@example.com", 0).status != 409 || e.post(l+"/update", url.Values{"choice": {"1"}}).status != 409 || e.post(l+"/remove", url.Values{}).status != 409 {
		t.Fatal("closed poll must reject answering, changing and removing")
	}
	if pub := e.get("/p/dinner").body; !strings.Contains(pub, "Mo, 12.10.2026, 14:00–15:30") || strings.Contains(pub, `name="email"`) || !strings.Contains(e.get(l).body, T("en", "outcome.title")) {
		t.Fatal("outcome not shown or form still there")
	}

	if r := e.get(e.manage("dinner")); !strings.Contains(r.body, e.manage("dinner")+"/reopen") {
		t.Fatal("re-open button missing")
	}
	mails := e.mailCount()
	if r := e.post(e.manage("dinner")+"/reopen", nil); r.header.Get("Location") != e.manage("dinner")+"?ok=reopened" || e.poll("dinner").Closed || e.mailCount() != mails {
		t.Fatalf("reopen: %d %s", r.status, r.header.Get("Location"))
	}
	if e.submit("dinner", "Late", "late@example.com", 0).status != 200 || strings.Contains(e.get(e.manage("dinner")).body, "/reopen") {
		t.Fatal("a re-opened poll takes answers and has no re-open button")
	}
	if e.post(e.manage("dinner")+"/close", url.Values{"option": {"1"}}); !e.poll("dinner").Closed || e.poll("dinner").Outcome != 1 {
		t.Fatal("close after re-open")
	}

	for _, c := range []struct {
		option, dtstart string
	}{{"2", ""}, {"3", "DTSTART:20261024T173000Z"}} {
		e := newEnv(t)
		e.createPoll("x", opts+"\nParty am 24.10. um 19.30 Uhr", true, false)
		e.srv.store.update("x", func(p *Poll) (string, error) { p.Options[3] = Option{Text: p.Options[3].Text}; return "test", nil })
		e.answer("x", "Anna", "anna@example.com", 2, 3)
		e.post(e.manage("x")+"/close", url.Values{"option": {c.option}})
		ms := e.mailsTo("anna@example.com")
		if m := ms[len(ms)-1]; (c.dtstart == "") != (m.ICS == nil) || !bytes.Contains(m.ICS, []byte(c.dtstart)) {
			t.Fatalf("option %s: ICS %s", c.option, m.ICS)
		}
	}
}

func TestAdmin(t *testing.T) {
	e := newEnv(t)
	for _, p := range []string{"/a", "/new"} {
		if e.get(p).status != 401 {
			t.Fatalf("%s without auth", p)
		}
	}
	if e.do("GET", "/a", nil, false, "Authorization", "Basic YWRtaW46d3Jvbmc=").status != 401 {
		t.Fatal("wrong password accepted")
	}
	e.createPoll("dinner", opts, false, false)
	if r := e.adminGet("/a"); !strings.Contains(r.body, `href="`+e.manage("dinner")+`"`) {
		t.Fatal("index misses poll")
	}
	if strings.Contains(e.get(e.manage("dinner")).body, `class="home logo"`) || strings.Contains(e.get("/p/dinner").body, `class="home logo"`) {
		t.Fatal("without a start page the logo links nowhere")
	}

	r := e.get(e.manage("dinner"))
	for _, want := range []string{`value="Team Dinner Ü"`, "Pizza</textarea>", `value="single" checked`} {
		if !strings.Contains(r.body, want) {
			t.Fatalf("edit form misses %q", want)
		}
	}

	r = e.post(e.manage("dinner")+"/edit", url.Values{"title": {""}, "description": {"kept"}, "options": {"A"}})
	if r.status != 400 || !strings.Contains(r.body, ">kept</textarea>") || e.poll("dinner").Title != "Team Dinner Ü" {
		t.Fatalf("invalid edit: %d", r.status)
	}

	e.post(e.manage("dinner")+"/edit", url.Values{"title": {"New"}, "options": {"A\nB"}, "mode": {"multiple"}, "results": {"1"}})
	if p := e.poll("dinner"); p.Title != "New" || len(p.Options) != 2 || !p.Multiple || !p.ResultsVisible {
		t.Fatal("edit failed")
	}
	if r := e.do("POST", e.manage("dinner")+"/edit", url.Values{"title": {""}}, false, "X-Autosave", "1"); r.status != 400 || strings.TrimSpace(r.body) != T("en", "err.title") {
		t.Fatalf("autosave error: %d %q", r.status, r.body)
	}

	l := e.answer("dinner", "Anna", "anna@example.com", 0)
	if e.post(e.manage("dinner")+"/edit", url.Values{"title": {"New"}, "options": {"X\nY\nZ"}}).status != 409 || len(e.poll("dinner").Options) != 2 {
		t.Fatal("options changed despite an answer")
	}
	if r := e.do("POST", e.manage("dinner")+"/edit", url.Values{"title": {"Renamed"}}, false, "X-Autosave", "1"); r.status != 204 || e.poll("dinner").Title != "Renamed" || e.poll("dinner").ResultsVisible {
		t.Fatalf("autosave: %d", r.status)
	}

	m := e.manage("dinner")
	if e.get(m+"/delete").status != 200 || !strings.Contains(e.post(m+"/delete", url.Values{}).body, T("en", "notice.deleted")) {
		t.Fatal("delete")
	}
	if _, err := e.srv.store.get("dinner"); err != errNotFound || e.get(l).status != 404 || e.post(m+"/delete", url.Values{}).status != 404 {
		t.Fatal("poll or participant link survives deletion")
	}
}

func TestDeleteRedirect(t *testing.T) {
	deleted := T("en", "notice.deleted")
	e := newEnv(t)
	e.createPoll("dinner", opts, false, false)
	if r := e.adminGet("/a").body; !strings.Contains(r, `action="`+e.manage("dinner")+`/delete" data-confirm="`) {
		t.Fatal("admin list must offer delete with a confirmation")
	}
	if r := e.adminPost(e.manage("dinner")+"/delete", url.Values{}); r.header.Get("Location") != "/a?ok=deleted" || !strings.Contains(e.adminGet("/a?ok=deleted").body, deleted) {
		t.Fatalf("admin must land on the admin page: %q", r.header.Get("Location"))
	}

	e = newEnv(t, func(c *config) { c.PublicCreate = true })
	e.createPoll("dinner", opts, false, false)
	if r := e.post(e.manage("dinner")+"/delete", url.Values{}); r.header.Get("Location") != "/?ok=deleted" || !strings.Contains(e.follow(r).body, deleted) {
		t.Fatalf("owner must land on the start page: %q", r.header.Get("Location"))
	}
	if strings.Contains(e.get("/").body, deleted) {
		t.Fatal("start page shows the notice only after a deletion")
	}
}

func TestLanguageSwitch(t *testing.T) {
	e := newEnv(t)
	e.createPoll("dinner", opts, false, false)
	r := e.get("/p/dinner?lang=de")
	if !strings.Contains(r.body, `<html lang="de">`) || !strings.Contains(r.body, `aria-current="true">de</a>`) || r.header.Get("Set-Cookie") != "" {
		t.Fatal("?lang=de must switch, without a cookie")
	}
	if r := e.get("/p/dinner?lang=xx"); !strings.Contains(r.body, `<html lang="en">`) {
		t.Fatal("unknown languages are ignored")
	}
	if got := T("en", "answer.yours", "Ken"); got != "Ken's answer" {
		t.Fatalf("answer heading: %q", got)
	}
}

func TestThemes(t *testing.T) {
	if got := strings.Join(themeNames(), ","); got != "business,elegant,fun,minimal" {
		t.Fatalf("built-in themes: %s", got)
	}

	e := newEnv(t, func(c *config) { c.Theme = "fun" })
	if css := e.get("/theme.css").body; !strings.Contains(css, "#16a34a") {
		t.Fatal("theme.css must be the instance theme")
	}

	e = newEnv(t)
	if r := e.adminGet("/new?theme=fun"); !strings.Contains(r.body, `href="/theme.css?theme=fun"`) || !strings.Contains(r.body, `<option value="fun" selected>`) {
		t.Fatal("preview")
	}
	if r := e.adminGet("/new").body; !strings.Contains(r, `<option value="minimal" selected>`) || !strings.Contains(r, `value="multiple" checked`) {
		t.Fatal("the instance theme must be preselected")
	}
	e.rename(createdID(e.adminPost("/new", url.Values{"title": {"T"}, "options": {"A"}, "theme": {"fun"}, "name": {"O"}, "email": {"o@example.com"}})), "t")
	if e.poll("t").Theme != "fun" || !strings.Contains(e.get("/p/t").body, `href="/theme.css?theme=fun"`) || !strings.Contains(e.get(e.manage("t")).body, "theme=fun") {
		t.Fatal("poll theme not stored or not used")
	}
	if r := e.submit("t", "Anna", "", 0); !strings.Contains(r.body, T("en", "sent.title")) || !strings.Contains(r.body, `href="/theme.css?theme=fun"`) {
		t.Fatal("the thanks page must use the poll's theme")
	}
	e.post(e.manage("t")+"/edit", url.Values{"title": {"T"}, "theme": {"bogus"}})
	if e.poll("t").Theme != "" {
		t.Fatal("unknown theme must fall back to the default")
	}
}
