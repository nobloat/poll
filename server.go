package main

import (
	"bytes"
	"cmp"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"log"
	"maps"
	"net"
	"net/http"
	"net/url"
	"path"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"sync"
	ttemplate "text/template"
	"time"
	"unicode/utf8"
)

//go:embed templates mail static i18n
var assets embed.FS

const (
	maxBody    = 64 << 10
	maxAnswers = 500 // per poll (owner decision 2026-11-20)
	// retentionDays after its last change a poll is deleted (owner decision 2026-11-20).
	retentionDays = 180
	// authFailures failed admin logins per client IP within authWindow answer 429 until the window passes.
	authFailures = 10
	authWindow   = 10 * time.Minute
)

var (
	errClosed    = errors.New("poll closed")
	errLocked    = errors.New("options locked")
	errInvalid   = errors.New("invalid input")
	errChoice    = errors.New("invalid choice")
	errPow       = errors.New("proof of work failed")
	errFull      = errors.New("poll full")
	errBusy      = errors.New("rate limited")
	errMailLimit = errors.New("mail rate limit")
)

type config struct {
	BaseURL        string
	AdminPassword  string
	AdminEmail     string
	Loc            *time.Location
	Difficulty     int
	MailDifficulty int // proof of work for requests that mail an address typed into the form
	MailPerAddress int // mails per hour to one address, 0: no limit
	MailPerHour    int // mails per hour in total, 0: no limit
	CreatePerHour  int // polls created per hour without admin auth, 0: no limit
	PublicCreate   bool
	Theme          string
	DataDir        string
	Addr           string
	SMTP           smtpConfig
}

type server struct {
	cfg       config
	store     *store
	pow       *pow
	now       func() time.Time
	outbox    *outbox
	pages     map[string]*template.Template
	mailText  *ttemplate.Template
	themes    map[string][]byte
	mails     limiter // per recipient and in total, per hour
	creations limiter // public creations per hour
	authFails limiter // failed admin logins per client IP
	changes   limiter // "changed" notifications per answer
}

func newServer(cfg config, now func() time.Time, send func(Mail) error) (*server, error) {
	st, err := openStore(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	if cfg.Theme == "" {
		cfg.Theme = "minimal"
	}
	s := &server{cfg: cfg, store: st, pow: newPow(cfg.Difficulty, now), now: now, pages: map[string]*template.Template{},
		mails: limiter{every: time.Hour}, creations: limiter{every: time.Hour}, authFails: limiter{every: authWindow},
		changes: limiter{every: 10 * time.Minute}}
	funcs := template.FuncMap{
		"t":       T,
		"in":      func(v url.Values, k, s string) bool { return slices.Contains(v[k], s) },
		"inc":     func(i int) int { return i + 1 },
		"list":    func(s ...string) []string { return s },
		"version": version,
		"langs":   func() []string { return slices.Sorted(maps.Keys(messages)) },
	}
	files, _ := fs.Glob(assets, "templates/*.html")
	for _, f := range files {
		if name := strings.TrimSuffix(path.Base(f), ".html"); name != "layout" {
			s.pages[name] = template.Must(template.New("").Funcs(funcs).ParseFS(assets, "templates/layout.html", f))
		}
	}
	s.mailText = ttemplate.Must(ttemplate.New("").Funcs(ttemplate.FuncMap(funcs)).ParseFS(assets, "mail/*.txt"))
	s.themes = map[string][]byte{}
	for _, name := range themeNames() {
		s.themes[name], _ = assets.ReadFile("static/themes/" + name + ".css")
	}
	s.outbox, err = openOutbox(cfg.DataDir, send)
	if err != nil {
		return nil, err
	}
	return s, nil
}

func (s *server) handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("GET /static/", http.FileServerFS(assets))
	mux.HandleFunc("GET /theme.css", s.theme)
	mux.HandleFunc("GET /pow", func(w http.ResponseWriter, r *http.Request) {
		bits := s.cfg.Difficulty
		if r.URL.Query().Has("mail") {
			bits = max(bits, s.cfg.MailDifficulty)
		}
		fmt.Fprintf(w, "%d %s", bits, s.pow.issue(bits))
	})
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		if s.cfg.PublicCreate {
			l := lang(r)
			s.render(w, r, http.StatusOK, "home", &page{Lang: l, Notice: deletedNotice(r)})
			return
		}
		http.Redirect(w, r, "/a", http.StatusSeeOther)
	})
	admin := func(pattern string, h http.HandlerFunc) { mux.Handle(pattern, s.auth(h)) }
	create := admin
	if s.cfg.PublicCreate {
		create = func(pattern string, h http.HandlerFunc) { mux.Handle(pattern, h) }
	}
	create("GET /new", s.newPoll)
	create("POST /new", s.createPoll)
	mux.HandleFunc("GET /p/{id}", s.pollPage)
	mux.HandleFunc("POST /p/{id}", s.submit)
	mux.HandleFunc("GET /p/{id}/a/{token}", s.answerPage)
	mux.HandleFunc("GET /p/{id}/a/{token}/thanks", s.thanks)
	mux.HandleFunc("GET /p/{id}/sent", s.sent)
	mux.HandleFunc("GET /p/{id}/removed", s.removed)
	mux.HandleFunc("POST /p/{id}/a/{token}/update", s.updateAnswer)
	mux.HandleFunc("GET /p/{id}/a/{token}/remove", s.removeConfirm)
	mux.HandleFunc("POST /p/{id}/a/{token}/remove", s.removeAnswer)
	admin("GET /a", s.adminIndex)
	s.manageRoutes(mux)
	return s.headers(http.NewCrossOriginProtection().Handler(s.proved(mux)))
}

func deletedNotice(r *http.Request) string {
	if r.URL.Query().Get("ok") == "deleted" {
		return "deleted"
	}
	return ""
}

func (s *server) proved(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost {
			if err := r.ParseForm(); err != nil {
				s.fail(w, r, errors.Join(errInvalid, err))
				return
			}
			if !s.pow.verify(r.PostForm.Get("pow"), r.PostForm.Get("nonce")) {
				s.fail(w, r, errPow)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (s *server) headers(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'; img-src 'self'; object-src 'none'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "DENY")
		h.Set("Referrer-Policy", "no-referrer")
		if !strings.HasPrefix(r.URL.Path, "/static/") {
			h.Set("Cache-Control", "no-store")
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxBody)
		next.ServeHTTP(w, r)
	})
}

func (s *server) isAdmin(r *http.Request) bool {
	want := sha256.Sum256([]byte("admin\x00" + s.cfg.AdminPassword))
	u, p, ok := r.BasicAuth()
	got := sha256.Sum256([]byte(u + "\x00" + p))
	return ok && subtle.ConstantTimeCompare(got[:], want[:]) == 1
}

func (s *server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip := clientIP(r)
		if s.authFails.over(s.now(), ip, authFailures) {
			s.message(w, r, http.StatusTooManyRequests, "busy.title", "busy.text", "", "")
			return
		}
		if !s.isAdmin(r) {
			if _, _, sent := r.BasicAuth(); sent {
				s.authFails.allow(s.now(), limit{ip, 0})
				log.Printf("admin login failed from %s", ip)
			}
			w.Header().Set("WWW-Authenticate", `Basic realm="][/poll", charset="UTF-8"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// clientIP is the address the proxy in front (caddy) put last into X-Forwarded-For, else the peer address.
// ponytail: trusts X-Forwarded-For; poll is meant to run behind a proxy that sets it (README), add a
// trusted-proxy list if it is ever exposed directly.
func clientIP(r *http.Request) string {
	if xff := r.Header.Values("X-Forwarded-For"); len(xff) > 0 {
		last := xff[len(xff)-1]
		return strings.TrimSpace(last[strings.LastIndexByte(last, ',')+1:])
	}
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	return host
}

var themeNames = sync.OnceValue(func() []string {
	files, _ := fs.Glob(assets, "static/themes/*.css")
	var names []string
	for _, f := range files {
		names = append(names, strings.TrimSuffix(path.Base(f), ".css"))
	}
	return names
})

func (s *server) themeFor(name string) []byte {
	if t, ok := s.themes[name]; ok {
		return t
	}
	return s.themes[s.cfg.Theme]
}

var release string

var version = sync.OnceValue(func() string {
	v := cmp.Or(release, "main")
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, s := range bi.Settings {
			if s.Key == "vcs.revision" && len(s.Value) >= 7 {
				v += " (" + s.Value[:7] + ")"
			}
		}
	}
	return v
})

func (s *server) theme(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/css; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=300")
	w.Write(s.themeFor(r.URL.Query().Get("theme")))
}

type result struct {
	Index  int
	Option Option
	Names  []string
	Max    int
	Lead   bool
}

func (r result) Bar() string { return strings.Repeat("■", len(r.Names)) }

func results(p *Poll) []result {
	rs := make([]result, len(p.Options))
	for i, o := range p.Options {
		rs[i] = result{Index: i, Option: o}
	}
	for _, a := range p.Answers {
		for _, c := range a.Choices {
			rs[c].Names = append(rs[c].Names, a.Name)
		}
	}
	top := 1
	for _, r := range rs {
		top = max(top, len(r.Names))
	}
	for i := range rs {
		rs[i].Max = top
		rs[i].Lead = len(rs[i].Names) == top
	}

	slices.SortStableFunc(rs, func(a, b result) int { return len(b.Names) - len(a.Names) })
	return rs
}

type page struct {
	Lang     string
	Home     string
	Base     string
	Public   bool
	Theme    string
	Themes   []string
	BaseURL  string
	Title    string
	Poll     *Poll
	Polls    []*Poll
	Answer   *Answer
	Results  []result
	Error    string
	Notice   string
	Form     url.Values
	Text     string
	Detail   string
	Action   string
	Button   string
	Link     string
	LinkText string
}

func lang(r *http.Request) string {
	if l := r.URL.Query().Get("lang"); messages[l] != nil {
		return l
	}
	return detectLang(r.Header.Get("Accept-Language"))
}

func (s *server) render(w http.ResponseWriter, r *http.Request, status int, name string, p *page) {
	if p.Lang == "" {
		p.Lang = lang(r)
	}

	if p.Poll != nil {
		p.Theme = p.Poll.Theme
	} else if p.Theme == "" {
		p.Theme = s.validTheme(r.URL.Query().Get("theme"))
	}
	if p.Form.Has("theme") {
		p.Theme = s.validTheme(p.Form.Get("theme"))
	}
	if p.Form != nil && p.Form.Get("theme") == "" {
		p.Form.Set("theme", s.cfg.Theme)
	}
	if s.cfg.PublicCreate {
		p.Home = "/"
	}
	p.BaseURL = s.cfg.BaseURL
	p.Themes = themeNames()
	var buf bytes.Buffer
	if err := s.pages[name].ExecuteTemplate(&buf, "layout", p); err != nil {
		log.Printf("render %s: %v", name, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(status)
	buf.WriteTo(w)
}

func autosave(r *http.Request) bool { return r.Header.Get("X-Autosave") == "1" }

func (s *server) message(w http.ResponseWriter, r *http.Request, status int, title, text, link, linkText string) {
	l := lang(r)
	if autosave(r) {
		http.Error(w, T(l, cmp.Or(text, title)), status)
		return
	}
	pg := &page{Lang: l, Title: T(l, title), Text: text, Link: link, LinkText: linkText}
	if p, err := s.store.get(r.PathValue("id")); err == nil {
		pg.Theme = p.Theme
	}
	s.render(w, r, status, "message", pg)
}

func (s *server) fail(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, errNotFound):
		s.message(w, r, http.StatusNotFound, "notfound.title", "", "", "")
	case errors.Is(err, errClosed):
		s.message(w, r, http.StatusConflict, "closed.title", "closed.text", "", "")
	case errors.Is(err, errLocked):
		s.message(w, r, http.StatusConflict, "locked.title", "locked.text", "", "")
	case errors.Is(err, errPow):
		s.message(w, r, http.StatusBadRequest, "invalid.title", "err.pow", "", "")
	case errors.Is(err, errFull):
		s.message(w, r, http.StatusConflict, "full.title", "full.text", "", "")
	case errors.Is(err, errBusy):
		s.message(w, r, http.StatusTooManyRequests, "busy.title", "busy.text", "", "")
	case errors.Is(err, errChoice):
		s.message(w, r, http.StatusBadRequest, "invalid.title", "err.choice", "", "")
	case errors.As(err, new(*http.MaxBytesError)):
		s.message(w, r, http.StatusRequestEntityTooLarge, "invalid.title", "", "", "")
	case errors.Is(err, errInvalid):
		s.message(w, r, http.StatusBadRequest, "invalid.title", "", "", "")
	default:
		log.Print(err)
		s.message(w, r, http.StatusInternalServerError, "error.title", "error.text", "", "")
	}
}

type mailData struct {
	Lang    string
	Poll    *Poll
	Answer  *Answer
	Link    string
	Event   string
	Choices []string
	ICS     bool
	Public  string
}

func (s *server) answerLink(p *Poll, a *Answer) string {
	return s.cfg.BaseURL + "/p/" + p.ID + "/a/" + a.Token
}

func (s *server) mail(to, name, subject string, d mailData, ics []byte) error {
	if to == "" {
		return nil
	}
	if !s.mails.allow(s.now(), limit{"to:" + to, s.cfg.MailPerAddress}, limit{"", s.cfg.MailPerHour}) {
		log.Printf("mail %s to=%q subject=%q: not queued, rate limit reached", name, to, subject)
		return errMailLimit
	}
	var txt bytes.Buffer
	err := s.mailText.ExecuteTemplate(&txt, name+".txt", d)
	if err == nil {
		// Mail templates may be checked out with CRLF (git autocrlf on Windows); the body is LF either way.
		text := strings.ReplaceAll(txt.String(), "\r\n", "\n")
		err = s.outbox.enqueue(Mail{To: to, Subject: subject, Text: text, ICS: ics})
	}
	if err != nil {
		log.Printf("mail %s to=%q subject=%q: %v", name, to, subject, err)
	}
	return err
}

func (s *server) mailLink(p *Poll, a *Answer) error {
	return s.mail(a.Email, "link", T(a.Lang, "mail.link.subject", p.Title), mailData{Lang: a.Lang, Poll: p, Answer: a, Link: s.answerLink(p, a)}, nil)
}

func (s *server) notifyOwner(p *Poll, a *Answer, event string) {
	var choices []string
	for _, c := range a.Choices {
		choices = append(choices, p.Options[c].Text)
	}
	s.mail(p.OwnerEmail, "admin", T(p.OwnerLang, "mail.admin.subject."+event, a.Name, p.Title),
		mailData{Lang: p.OwnerLang, Poll: p, Answer: a, Event: event, Choices: choices, Link: s.ownerLink(p)}, nil)
}

func (s *server) mailClosed(p *Poll, a *Answer) {
	var ics []byte
	o := p.OutcomeOption()
	if !o.HasDate() {
		o = parseOption(o.Text, s.cfg.Loc, s.now())
	}
	if o.HasDate() {
		u, _ := url.Parse(s.cfg.BaseURL)
		ics = buildICS(p.ID+"@"+u.Hostname(), p.Title, p.Description, o, s.now())
	}
	s.mail(a.Email, "closed", T(a.Lang, "mail.closed.subject", p.Title), mailData{Lang: a.Lang, Poll: p, Answer: a, Link: s.answerLink(p, a), ICS: ics != nil}, ics)
}

func (s *server) pollPage(w http.ResponseWriter, r *http.Request) {
	p, err := s.store.get(r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	pg := &page{Poll: p}
	if p.ResultsVisible {
		pg.Results = results(p)
	}
	s.render(w, r, http.StatusOK, "poll", pg)
}

func parseChoices(p *Poll, vals []string) ([]int, bool) {
	var cs []int
	for _, v := range vals {
		i, err := strconv.Atoi(v)
		if err != nil || i < 0 || i >= len(p.Options) || slices.Contains(cs, i) {
			return nil, false
		}
		cs = append(cs, i)
	}
	slices.Sort(cs)
	return cs, len(cs) <= 1 || p.Multiple
}

func (s *server) submit(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	p, err := s.store.get(id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	f := r.PostForm
	name := strings.TrimSpace(f.Get("name"))
	email := strings.ToLower(strings.TrimSpace(f.Get("email")))
	choices, okChoices := parseChoices(p, f["choice"])
	var errKey string
	switch {
	case p.Closed:
		s.fail(w, r, errClosed)
		return
	case !validName(name):
		errKey = "err.name"
	case email != "" && !validEmail(email):
		errKey = "err.email"
	case !okChoices:
		errKey = "err.choice"
	}
	if errKey != "" {
		pg := &page{Poll: p, Error: errKey, Form: f}
		if p.ResultsVisible {
			pg.Results = results(p)
		}
		s.render(w, r, http.StatusBadRequest, "poll", pg)
		return
	}
	if email != "" && !s.mailProved(r) {
		s.fail(w, r, errPow)
		return
	}
	var added, existing *Answer
	p, err = s.store.update(id, func(p *Poll) (string, error) {
		if p.Closed {
			return "", errClosed
		}
		if existing = p.byEmail(email); email != "" && existing != nil {
			return "", errExists
		}
		existing = nil
		if len(p.Answers) >= maxAnswers {
			return "", errFull
		}
		if choices, okChoices = parseChoices(p, f["choice"]); !okChoices {
			return "", errChoice
		}
		added = &Answer{Token: rand.Text(), Name: name, Email: email, Lang: lang(r), Choices: choices, Created: s.now()}
		p.Answers = append(p.Answers, added)
		return fmt.Sprintf("answer %v added", added), nil
	})
	switch {
	case existing != nil:
		// The same answer as for a new address, so the page tells nobody whether an address took part; the
		// address gets its existing link again.
		log.Printf("poll %s: answer %v not added again, email already answered; link mailed again", id, existing)
		s.mailLink(p, existing)
	case err != nil:
		s.fail(w, r, err)
		return
	default:
		s.mailLink(p, added)
		s.notifyOwner(p, added, "new")
	}
	if email == "" {
		http.Redirect(w, r, "/p/"+p.ID+"/a/"+added.Token+"/thanks", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/p/"+p.ID+"/sent", http.StatusSeeOther)
}

func validName(name string) bool {
	return name != "" && utf8.RuneCountInString(name) <= 100 && singleLine(name)
}

// mailProved reports whether the request's proof of work has the difficulty for sending mail to a typed address.
func (s *server) mailProved(r *http.Request) bool {
	return challengeBits(r.PostForm.Get("pow")) >= s.cfg.MailDifficulty
}

func (s *server) sent(w http.ResponseWriter, r *http.Request) {
	s.message(w, r, http.StatusOK, "sent.title", "sent.mail", "", "")
}

func (s *server) thanks(w http.ResponseWriter, r *http.Request) {
	p, a := s.lookup(w, r)
	if a == nil {
		return
	}
	text := "sent.text"
	if a.Email == "" {
		text = "sent.text.nomail"
	}
	s.message(w, r, http.StatusOK, "sent.title", text, "/p/"+p.ID+"/a/"+a.Token, "sent.link")
}

func (s *server) removed(w http.ResponseWriter, r *http.Request) {
	s.message(w, r, http.StatusOK, "removed.title", "removed.text", "/p/"+r.PathValue("id"), "removed.back")
}

func (s *server) lookup(w http.ResponseWriter, r *http.Request) (*Poll, *Answer) {
	p, err := s.store.get(r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return nil, nil
	}
	a := p.byToken(r.PathValue("token"))
	if a == nil {
		s.fail(w, r, errNotFound)
		return nil, nil
	}
	return p, a
}

func (s *server) answerPage(w http.ResponseWriter, r *http.Request) {
	p, a := s.lookup(w, r)
	if a == nil {
		return
	}
	pg := &page{Poll: p, Answer: a}
	if r.URL.Query().Get("ok") == "updated" {
		pg.Notice = "saved"
	}
	if p.ResultsVisible {
		pg.Results = results(p)
	}
	s.render(w, r, http.StatusOK, "answer", pg)
}

func (s *server) act(r *http.Request, fn func(p *Poll, a *Answer) (string, error)) (*Poll, *Answer, error) {
	var ans *Answer
	p, err := s.store.update(r.PathValue("id"), func(p *Poll) (string, error) {
		if ans = p.byToken(r.PathValue("token")); ans == nil {
			return "", errNotFound
		}
		before := ans.String()
		action, err := fn(p, ans)
		return "answer " + before + " " + action, err
	})
	return p, ans, err
}

func (s *server) updateAnswer(w http.ResponseWriter, r *http.Request) {
	changed := false
	p, a, err := s.act(r, func(p *Poll, a *Answer) (string, error) {
		if p.Closed {
			return "", errClosed
		}
		cs, ok := parseChoices(p, r.PostForm["choice"])
		if !ok {
			return "", errChoice
		}
		changed = !slices.Equal(a.Choices, cs)
		a.Choices = cs
		return fmt.Sprintf("changed by participant to %v", cs), nil
	})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	// Autosave posts on every click: notify only on a real change, at most once per answer per 10 minutes.
	if changed && s.changes.allow(s.now(), limit{a.Token, 1}) {
		s.notifyOwner(p, a, "changed")
	}
	if autosave(r) {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	http.Redirect(w, r, "/p/"+p.ID+"/a/"+a.Token+"?ok=updated", http.StatusSeeOther)
}

func (s *server) removeConfirm(w http.ResponseWriter, r *http.Request) {
	p, a := s.lookup(w, r)
	if a == nil {
		return
	}
	if p.Closed {
		s.fail(w, r, errClosed)
		return
	}
	s.render(w, r, http.StatusOK, "confirm", &page{Poll: p, Text: "remove.text", Action: s.answerLink(p, a) + "/remove", Button: "remove.button", Link: s.answerLink(p, a), LinkText: "remove.cancel"})
}

func (s *server) removeAnswer(w http.ResponseWriter, r *http.Request) {
	p, a, err := s.act(r, func(p *Poll, a *Answer) (string, error) {
		if p.Closed {
			return "", errClosed
		}
		p.remove(a.Token)
		return "removed by participant", nil
	})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.notifyOwner(p, a, "removed")
	http.Redirect(w, r, "/p/"+p.ID+"/removed", http.StatusSeeOther)
}
