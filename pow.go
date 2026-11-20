package main

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"math/bits"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	powTTL = 10 * time.Minute
	// ponytail: fail closed when this many proofs were spent within one or two TTLs; raise it if real traffic hits it.
	powMaxUsed = 100_000
)

// pow hands out HMAC-signed challenges "issued.bits.rand.mac"; bits is the difficulty a challenge asks for, at least
// difficulty. Spent challenges sit in two maps rotated every TTL, so replay checks never walk the set.
type pow struct {
	key        []byte
	difficulty int
	now        func() time.Time
	mu         sync.Mutex
	used, prev map[string]bool
	rotated    time.Time
}

func newPow(difficulty int, now func() time.Time) *pow {
	key := make([]byte, 32)
	rand.Read(key)
	return &pow{key: key, difficulty: difficulty, now: now, used: map[string]bool{}}
}

func (p *pow) mac(msg string) string {
	h := hmac.New(sha256.New, p.key)
	h.Write([]byte(msg))
	return hex.EncodeToString(h.Sum(nil))
}

func (p *pow) issue(bits int) string {
	msg := strconv.FormatInt(p.now().Unix(), 10) + "." + strconv.Itoa(max(bits, p.difficulty)) + "." + rand.Text()
	return msg + "." + p.mac(msg)
}

// challengeBits is the difficulty a challenge asks for, -1 if it has none. Trust it only after verify.
func challengeBits(challenge string) int {
	f := strings.Split(challenge, ".")
	if len(f) != 4 {
		return -1
	}
	n, err := strconv.Atoi(f[1])
	if err != nil || n < 0 || n > 64 {
		return -1
	}
	return n
}

func leadingZeroBits(b []byte) int {
	n := 0
	for _, c := range b {
		n += bits.LeadingZeros8(c)
		if c != 0 {
			break
		}
	}
	return n
}

func (p *pow) verify(challenge, nonce string) bool {
	i := strings.LastIndexByte(challenge, '.')
	if i < 0 || len(nonce) == 0 || len(nonce) > 32 || !hmac.Equal([]byte(challenge[i+1:]), []byte(p.mac(challenge[:i]))) {
		return false
	}
	ts, err := strconv.ParseInt(challenge[:strings.IndexByte(challenge, '.')], 10, 64)
	issued, now := time.Unix(ts, 0), p.now()
	if err != nil || now.Sub(issued) >= powTTL || issued.After(now.Add(time.Minute)) {
		return false
	}
	need := challengeBits(challenge)
	sum := sha256.Sum256([]byte(challenge + nonce))
	if need < p.difficulty || leadingZeroBits(sum[:]) < need {
		return false
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if now.Sub(p.rotated) >= powTTL {
		p.prev, p.used, p.rotated = p.used, map[string]bool{}, now
	}
	if p.used[challenge] || p.prev[challenge] || len(p.used) >= powMaxUsed {
		return false
	}
	p.used[challenge] = true
	return true
}
