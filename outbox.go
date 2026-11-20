package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net/textproto"
	"os"
	"path/filepath"
	"sync"
	"time"
)

var errOutboxStopped = errors.New("mail outbox stopped")

// outboxGiveUp is how long a mail is retried before it moves to failed/ (owner decision 2026-11-20).
const outboxGiveUp = 3 * 24 * time.Hour

type mailJob struct {
	Mail     Mail      `json:"mail"`
	Attempts int       `json:"attempts"`
	Next     time.Time `json:"next"`
	Created  time.Time `json:"created,omitzero"`
}

// outbox queues mails as files in data/mail. The directory is read once at start; after that the due time of
// every queued file is kept in memory, so sending a mail reads only that mail's file.
type outbox struct {
	dir  string
	send func(Mail) error

	mu   sync.Mutex
	due  map[string]time.Time
	wake chan struct{}
	stop chan struct{}
	done chan struct{}
	once sync.Once
}

func openOutbox(dataDir string, send func(Mail) error) (*outbox, error) {
	dir := filepath.Join(dataDir, "mail")
	if err := os.MkdirAll(filepath.Join(dir, "failed"), 0o700); err != nil {
		return nil, err
	}
	if tmps, _ := filepath.Glob(filepath.Join(dir, ".tmp-*")); tmps != nil {
		for _, f := range tmps {
			os.Remove(f)
		}
	}
	q := &outbox{dir: dir, send: send, due: map[string]time.Time{}, wake: make(chan struct{}, 1), stop: make(chan struct{}), done: make(chan struct{})}
	files, err := filepath.Glob(filepath.Join(dir, "mail-*.json"))
	if err != nil {
		return nil, err
	}
	for _, f := range files {
		if job, ok := q.load(f); ok {
			q.due[f] = job.Next
		}
	}
	if len(q.due) > 0 {
		log.Printf("mail outbox: resuming %d queued mails", len(q.due))
	}
	go q.run()
	return q, nil
}

// load reads one queued mail; an unreadable one is renamed to .invalid and skipped.
func (q *outbox) load(f string) (mailJob, bool) {
	var job mailJob
	b, err := os.ReadFile(f)
	if err == nil {
		err = json.Unmarshal(b, &job)
	}
	if err == nil && !validEmail(job.Mail.To) {
		err = errors.New("invalid recipient")
	}
	if err != nil {
		log.Printf("mail outbox: invalid job %s: %v", filepath.Base(f), err)
		os.Rename(f, f+".invalid")
		return job, false
	}
	return job, true
}

func (q *outbox) enqueue(m Mail) error {
	select {
	case <-q.stop:
		return errOutboxStopped
	default:
	}
	q.mu.Lock()
	defer q.mu.Unlock()
	job := mailJob{Mail: m, Created: time.Now()}
	b, err := json.Marshal(job)
	if err != nil {
		return err
	}
	var name string
	for {
		name = filepath.Join(q.dir, "mail-"+newID()+".json")
		if _, err := os.Stat(name); errors.Is(err, fs.ErrNotExist) {
			break
		} else if err != nil {
			return err
		}
	}
	if err := writeOutboxFile(q.dir, name, b); err != nil {
		return err
	}
	q.due[name] = time.Time{}
	log.Printf("mail outbox: queued %s to=%q subject=%q", filepath.Base(name), m.To, m.Subject)
	q.signal()
	return nil
}

func writeOutboxFile(dir, path string, b []byte) error {
	f, err := os.CreateTemp(dir, ".tmp-")
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
	if err := os.Rename(f.Name(), path); err != nil {
		return err
	}
	return syncOutboxDir(dir)
}

func (q *outbox) pending() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return len(q.due) > 0
}

func (q *outbox) signal() {
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

// next returns the first due mail, or how long to wait for one.
// ponytail: scans the in-memory index, O(queued) per mail; a heap if the queue ever holds many thousands.
func (q *outbox) next(now time.Time) (string, mailJob, time.Duration) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for {
		var path string
		var soonest time.Time
		for f, t := range q.due {
			if path == "" || t.Before(soonest) {
				path, soonest = f, t
			}
		}
		if path == "" {
			return "", mailJob{}, time.Hour
		}
		if soonest.After(now) {
			return "", mailJob{}, soonest.Sub(now)
		}
		job, ok := q.load(path)
		if ok {
			return path, job, 0
		}
		delete(q.due, path)
	}
}

// permanent reports an SMTP 5xx reply to MAIL, RCPT or DATA (dial and auth errors are wrapped by send, so a
// misconfigured server never drops mail).
func permanent(err error) bool {
	var te *textproto.Error
	return errors.As(err, &te) && te.Code >= 500 && te.Code < 600
}

func (q *outbox) finish(path string, job mailJob, err error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	name := filepath.Base(path)
	if err == nil {
		delete(q.due, path)
		if removeErr := os.Remove(path); removeErr != nil && !errors.Is(removeErr, fs.ErrNotExist) {
			log.Printf("mail outbox: remove %s: %v", name, removeErr)
			return
		}
		if err := syncOutboxDir(q.dir); err != nil {
			log.Printf("mail outbox: sync: %v", err)
		}
		log.Printf("mail outbox: sent %s to=%q subject=%q", name, job.Mail.To, job.Mail.Subject)
		return
	}
	now := time.Now()
	if job.Created.IsZero() {
		job.Created = now
	}
	if permanent(err) || now.Sub(job.Created) >= outboxGiveUp {
		delete(q.due, path)
		if moveErr := os.Rename(path, filepath.Join(q.dir, "failed", name)); moveErr != nil {
			log.Printf("mail outbox: move %s to failed: %v", name, moveErr)
		}
		log.Printf("mail outbox: gave up %s to=%q subject=%q after %d attempts: %v", name, job.Mail.To, job.Mail.Subject, job.Attempts+1, err)
		return
	}
	job.Attempts++
	job.Next = now.Add(retryDelay(job.Attempts))
	q.due[path] = job.Next
	b, marshalErr := json.Marshal(job)
	if marshalErr != nil {
		log.Printf("mail outbox: encode %s: %v", name, marshalErr)
		return
	}
	if err := writeOutboxFile(q.dir, path, b); err != nil {
		log.Printf("mail outbox: save retry %s to=%q subject=%q: %v", name, job.Mail.To, job.Mail.Subject, err)
		return
	}
	log.Printf("mail outbox: retry %s to=%q subject=%q in %s: %v", name, job.Mail.To, job.Mail.Subject, retryDelay(job.Attempts), err)
}

func retryDelay(attempts int) time.Duration {
	return time.Minute * time.Duration(1<<min(max(attempts, 1)-1, 6))
}

func (q *outbox) run() {
	defer close(q.done)
	for {
		select {
		case <-q.stop:
			return
		default:
		}
		path, job, wait := q.next(time.Now())
		if path != "" {
			q.finish(path, job, q.send(job.Mail))
			continue
		}
		timer := time.NewTimer(max(wait, time.Millisecond))
		select {
		case <-q.stop:
			timer.Stop()
			return
		case <-q.wake:
			timer.Stop()
		case <-timer.C:
		}
	}
}

func syncOutboxDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

func (q *outbox) shutdown(ctx context.Context) error {
	q.once.Do(func() { close(q.stop) })
	select {
	case <-q.done:
		return nil
	case <-ctx.Done():
		return fmt.Errorf("mail outbox: %w", ctx.Err())
	}
}
