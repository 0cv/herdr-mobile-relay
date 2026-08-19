package gateway

import (
	"sync"
	"time"
)

// rateLimiter is a fixed-window counter keyed by client address. Fixed windows
// are chosen over token buckets because the gateway only needs to blunt floods,
// and a window needs one timestamp and one integer per key.
type rateLimiter struct {
	limit  int
	window time.Duration

	mu        sync.Mutex
	buckets   map[string]*rateBucket
	lastPrune time.Time
}

type rateBucket struct {
	start time.Time
	count int
}

// newRateLimiter builds a limiter of limit events per window. A limit of zero or
// less disables the limiter.
func newRateLimiter(limit int, window time.Duration) *rateLimiter {
	return &rateLimiter{
		limit:   limit,
		window:  window,
		buckets: make(map[string]*rateBucket),
	}
}

// allow charges one event against key and reports whether it may proceed.
func (l *rateLimiter) allow(key string, now time.Time) bool {
	if l.limit <= 0 {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()

	if now.Sub(l.lastPrune) >= l.window {
		l.pruneLocked(now)
		l.lastPrune = now
	}

	bucket := l.buckets[key]
	if bucket == nil || now.Sub(bucket.start) >= l.window {
		l.buckets[key] = &rateBucket{start: now, count: 1}
		return true
	}
	if bucket.count >= l.limit {
		return false
	}
	bucket.count++
	return true
}

// pruneLocked drops windows that can no longer deny anything.
func (l *rateLimiter) pruneLocked(now time.Time) {
	for key, bucket := range l.buckets {
		if now.Sub(bucket.start) >= 2*l.window {
			delete(l.buckets, key)
		}
	}
}
