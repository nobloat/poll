package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"
)

func loadConfig() (config, error) {
	var errs []error
	bad := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }
	req := func(key string) string {
		v := os.Getenv(key)
		if v == "" {
			bad("%s is required", key)
		}
		return v
	}
	s := config{
		BaseURL:        strings.TrimRight(req("BASE_URL"), "/"),
		AdminPassword:  req("ADMIN_PASSWORD"),
		AdminEmail:     strings.ToLower(strings.TrimSpace(os.Getenv("ADMIN_EMAIL"))),
		Loc:            time.Local,
		Difficulty:     16,
		MailDifficulty: 20,
		MailPerAddress: 5,
		MailPerHour:    200,
		CreatePerHour:  100,
		DataDir:        "./data",
		Addr:           ":" + cmp.Or(os.Getenv("PORT"), "8737"),
		Theme:          cmp.Or(os.Getenv("THEME"), "minimal"),
		SMTP:           smtpConfig{From: req("SMTP_FROM")},
	}
	if e := s.AdminEmail; e != "" && !validEmail(e) {
		bad("ADMIN_EMAIL is not a valid address: %q", e)
	}
	if v := os.Getenv("PUBLIC_CREATE"); v != "" {
		var err error
		if s.PublicCreate, err = strconv.ParseBool(v); err != nil {
			bad("PUBLIC_CREATE must be true or false, got %q", v)
		}
	}
	if e := s.SMTP.From; e != "" && !validEmail(e) {
		bad("SMTP_FROM is not a valid address: %q", e)
	}

	if v := req("SMTP_URL"); v != "" {
		u, err := url.Parse(v)
		tls := map[string]string{"smtp": "starttls", "smtp+insecure": "none"}
		if err != nil || tls[u.Scheme] == "" || u.Port() == "" {
			bad("SMTP_URL must be smtp://[user:pass@]host:port or smtp+insecure://host:port")
		} else {
			pass, _ := u.User.Password()
			s.SMTP.Addr, s.SMTP.User, s.SMTP.Password, s.SMTP.TLS = u.Host, u.User.Username(), pass, tls[u.Scheme]
		}
	}
	if !slices.Contains(themeNames(), s.Theme) {
		bad("THEME must be one of %s, got %q", strings.Join(themeNames(), ", "), s.Theme)
	}
	return s, errors.Join(errs...)
}

func main() {
	if os.Getuid() == 0 {
		log.Fatal("refusing to run as root; use an unprivileged user (the Docker image runs as 65532)")
	}
	cfg, err := loadConfig()
	if err != nil {
		log.Fatalf("invalid config:\n%v", err)
	}
	s, err := newServer(cfg, time.Now, cfg.SMTP.send)
	if err != nil {
		log.Fatal(err)
	}
	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           s.handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      5 * time.Minute,
		IdleTimeout:       120 * time.Second,
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() { // delete polls 180 days after their last change, at start and then daily
		for {
			s.store.sweep(time.Now().AddDate(0, 0, -retentionDays))
			select {
			case <-ctx.Done():
				return
			case <-time.After(24 * time.Hour):
			}
		}
	}()
	go func() {
		if err := srv.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			log.Fatal(err)
		}
	}()
	log.Printf("nobloat/poll listening on %s (%s, theme %s, public create %v, time zone %s)", cfg.Addr, cfg.BaseURL, cfg.Theme, cfg.PublicCreate, cfg.Loc)
	<-ctx.Done()
	log.Print("shutting down, waiting up to 8s for running requests")
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Printf("shutdown: %v", err)
	}
	if err := s.outbox.shutdown(ctx); err != nil {
		log.Printf("shutdown: %v", err)
	}
	log.Print("stopped")
}
