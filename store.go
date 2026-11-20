package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

var (
	errNotFound = errors.New("not found")
	errExists   = errors.New("already exists")
)

type Option struct {
	Text   string    `json:"text"`
	Start  time.Time `json:"start,omitzero"`
	End    time.Time `json:"end,omitzero"`
	AllDay bool      `json:"allDay,omitempty"`
}

func (o Option) HasDate() bool { return !o.Start.IsZero() }

type Answer struct {
	Token   string    `json:"token"`
	Name    string    `json:"name"`
	Email   string    `json:"email"`
	Lang    string    `json:"lang"`
	Choices []int     `json:"choices,omitempty"`
	Created time.Time `json:"created,omitzero"`
}

func (a *Answer) Chose(i int) bool { return slices.Contains(a.Choices, i) }

func (a *Answer) String() string { return fmt.Sprintf("%q choices %v", a.Name, a.Choices) }

func (p *Poll) None() (as []*Answer) {
	for _, a := range p.Answers {
		if len(a.Choices) == 0 {
			as = append(as, a)
		}
	}
	return as
}

type Poll struct {
	ID             string    `json:"-"`
	Title          string    `json:"title"`
	Description    string    `json:"description"`
	Multiple       bool      `json:"multiple"`
	ResultsVisible bool      `json:"resultsVisible"`
	Options        []Option  `json:"options"`
	Answers        []*Answer `json:"answers"`
	Closed         bool      `json:"closed"`
	Outcome        int       `json:"outcome"`
	Created        time.Time `json:"created"`

	OwnerName  string `json:"owner_name,omitempty"`
	OwnerEmail string `json:"owner_email,omitempty"`
	OwnerLang  string `json:"owner_lang,omitempty"`
	OwnerToken string `json:"owner_token,omitempty"`
	Theme      string `json:"theme,omitempty"`
}

func (p *Poll) byToken(token string) *Answer {
	for _, a := range p.Answers {
		if a.Token == token {
			return a
		}
	}
	return nil
}

func (p *Poll) byEmail(email string) *Answer {
	for _, a := range p.Answers {
		if a.Email == email {
			return a
		}
	}
	return nil
}

func (p *Poll) remove(token string) {
	p.Answers = slices.DeleteFunc(p.Answers, func(a *Answer) bool { return a.Token == token })
}

func (p *Poll) OutcomeOption() Option { return p.Options[p.Outcome] }

func (p *Poll) OptionsText() string {
	lines := make([]string, len(p.Options))
	for i, o := range p.Options {
		lines[i] = o.Text
	}
	return strings.Join(lines, "\n")
}

// store keeps one JSON file per poll. Writes are atomic renames, so reads take no lock; changes to one poll are
// serialised by one of a fixed set of mutexes picked by its ID.
type store struct {
	dir   string
	locks [64]sync.Mutex
}

func (s *store) lock(id string) (unlock func()) {
	h := fnv.New32a()
	h.Write([]byte(id))
	mu := &s.locks[h.Sum32()%uint32(len(s.locks))]
	mu.Lock()
	return mu.Unlock
}

func openStore(dataDir string) (*store, error) {
	dir := filepath.Join(dataDir, "polls")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	tmps, _ := filepath.Glob(filepath.Join(dir, ".tmp-*"))
	for _, f := range tmps {
		os.Remove(f)
	}
	return &store{dir: dir}, nil
}

func (s *store) path(id string) string { return filepath.Join(s.dir, id+".json") }

func (s *store) read(id string) (*Poll, error) {
	if !validID(id) {
		return nil, errNotFound
	}
	b, err := os.ReadFile(s.path(id))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, errNotFound
	}
	if err != nil {
		return nil, err
	}
	p := Poll{ID: id}
	return &p, json.Unmarshal(b, &p)
}

func (s *store) write(p *Poll) error {
	b, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(s.dir, ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(f.Name(), s.path(p.ID)); err != nil {
		return err
	}
	return s.syncDir()
}

func (s *store) get(id string) (*Poll, error) { return s.read(id) }

func (s *store) update(id string, fn func(*Poll) (string, error)) (*Poll, error) {
	if !validID(id) {
		return nil, errNotFound
	}
	defer s.lock(id)()
	p, err := s.read(id)
	if err != nil {
		return nil, err
	}
	action, err := fn(p)
	if err != nil {
		return p, err
	}
	return p, s.logged(id, action, s.write(p))
}

func (s *store) logged(id, action string, err error) error {
	if err == nil {
		log.Printf("poll %s: %s", id, action)
	}
	return err
}

func (s *store) create(p *Poll, action string) error {
	defer s.lock(p.ID)()
	if _, err := os.Stat(s.path(p.ID)); !errors.Is(err, fs.ErrNotExist) {
		if err == nil {
			return errExists
		}
		return err
	}
	return s.logged(p.ID, action, s.write(p))
}

func (s *store) delete(id string) error {
	if !validID(id) {
		return errNotFound
	}
	defer s.lock(id)()
	p, err := s.read(id)
	if err != nil {
		return err
	}
	action := fmt.Sprintf("deleted %q with %d answers", p.Title, len(p.Answers))
	if err := os.Remove(s.path(id)); errors.Is(err, fs.ErrNotExist) {
		return errNotFound
	} else if err != nil {
		return err
	}
	return s.logged(id, action, s.syncDir())
}

func (s *store) syncDir() error {
	d, err := os.Open(s.dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// list returns every readable poll; an unreadable or stray file is logged and skipped.
func (s *store) list() ([]*Poll, error) {
	files, err := filepath.Glob(filepath.Join(s.dir, "*.json"))
	if err != nil {
		return nil, err
	}
	var polls []*Poll
	for _, f := range files {
		p, err := s.read(strings.TrimSuffix(filepath.Base(f), ".json"))
		if err != nil {
			log.Printf("poll list: skipping %s: %v", filepath.Base(f), err)
			continue
		}
		polls = append(polls, p)
	}
	slices.SortFunc(polls, func(a, b *Poll) int { return b.Created.Compare(a.Created) })
	return polls, nil
}

// sweep deletes every poll whose file was last written (created, answered, edited, closed) before cutoff.
func (s *store) sweep(cutoff time.Time) {
	files, err := filepath.Glob(filepath.Join(s.dir, "*.json"))
	if err != nil {
		log.Printf("poll sweep: %v", err)
		return
	}
	for _, f := range files {
		id := strings.TrimSuffix(filepath.Base(f), ".json")
		unlock := s.lock(id)
		if fi, err := os.Stat(f); err == nil && fi.ModTime().Before(cutoff) {
			if err := os.Remove(f); err != nil {
				log.Printf("poll sweep: %v", err)
			} else {
				log.Printf("poll %s: deleted, no activity since %s", id, fi.ModTime().Format(time.DateOnly))
			}
		}
		unlock()
	}
	if err := s.syncDir(); err != nil {
		log.Printf("poll sweep: %v", err)
	}
}
