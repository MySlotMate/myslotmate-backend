// Package ratelimit provides a small in-memory, per-key token-bucket limiter.
// It is used to throttle private-event passkey attempts (the unlock endpoint and
// the booking passkey path) so a paid, passkey-comped event can't be brute-forced
// into a free ticket. In-memory means the limit is per process instance — good
// enough to make brute force impractical; a shared store would be needed for a
// hard global cap across instances.
package ratelimit

import (
	"sync"
	"time"

	"golang.org/x/time/rate"
)

type entry struct {
	lim  *rate.Limiter
	seen time.Time
}

// KeyedLimiter hands each key its own token bucket; idle keys are evicted.
type KeyedLimiter struct {
	mu      sync.Mutex
	buckets map[string]*entry
	r       rate.Limit
	burst   int
}

// NewKeyedLimiter allows up to perMinute sustained requests per key with the
// given burst. A background sweeper evicts keys idle for 15 minutes.
func NewKeyedLimiter(perMinute, burst int) *KeyedLimiter {
	kl := &KeyedLimiter{
		buckets: make(map[string]*entry),
		r:       rate.Every(time.Minute / time.Duration(perMinute)),
		burst:   burst,
	}
	go kl.cleanupLoop()
	return kl
}

// Allow reports whether the key may proceed, consuming one token when it can.
func (kl *KeyedLimiter) Allow(key string) bool {
	kl.mu.Lock()
	defer kl.mu.Unlock()
	e, ok := kl.buckets[key]
	if !ok {
		e = &entry{lim: rate.NewLimiter(kl.r, kl.burst)}
		kl.buckets[key] = e
	}
	e.seen = time.Now()
	return e.lim.Allow()
}

func (kl *KeyedLimiter) cleanupLoop() {
	t := time.NewTicker(10 * time.Minute)
	defer t.Stop()
	for range t.C {
		kl.mu.Lock()
		for k, e := range kl.buckets {
			if time.Since(e.seen) > 15*time.Minute {
				delete(kl.buckets, k)
			}
		}
		kl.mu.Unlock()
	}
}

// Passkey throttles private-event passkey attempts, keyed by client IP + event.
// ~10 attempts/minute per IP+event makes brute force impractical while never
// getting in a real guest's way. Shared by the unlock and booking paths.
var Passkey = NewKeyedLimiter(10, 10)
