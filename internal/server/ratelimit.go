package server

import (
	"math"
	"net"
	"net/http"
	"sync"
	"time"
)

// limiter is an in-memory token bucket per key: at most rate requests in a
// burst, refilled at rate per window. Limits reset when the process
// restarts, which is acceptable for bounding abuse of one aicrewd.
type limiter struct {
	mu      sync.Mutex
	rate    float64
	window  time.Duration
	now     func() time.Time
	buckets map[string]*bucket
}

type bucket struct {
	tokens float64
	last   time.Time
}

// maxBuckets bounds the memory the limiter uses; see allow.
const maxBuckets = 4096

func newLimiter(rate int, window time.Duration) *limiter {
	return &limiter{rate: float64(rate), window: window, now: time.Now, buckets: map[string]*bucket{}}
}

// allow takes one token for key. When none is left it returns false and how
// long until the next token.
func (l *limiter) allow(key string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	b, ok := l.buckets[key]
	if !ok {
		if len(l.buckets) >= maxBuckets {
			l.prune(now)
		}
		b = &bucket{tokens: l.rate, last: now}
		l.buckets[key] = b
	}
	l.refill(b, now)
	if b.tokens < 1 {
		wait := time.Duration(math.Ceil((1 - b.tokens) * float64(l.window) / l.rate))
		return false, wait
	}
	b.tokens--
	return true, 0
}

func (l *limiter) refill(b *bucket, now time.Time) {
	if elapsed := now.Sub(b.last); elapsed > 0 {
		b.tokens = math.Min(l.rate, b.tokens+elapsed.Seconds()*l.rate/l.window.Seconds())
		b.last = now
	}
}

// prune drops the buckets that have refilled, which carry no state. If every
// bucket is still draining, the map is cleared: past maxBuckets distinct
// clients a per-client limit no longer bounds much anyway.
func (l *limiter) prune(now time.Time) {
	for k, b := range l.buckets {
		l.refill(b, now)
		if b.tokens >= l.rate {
			delete(l.buckets, k)
		}
	}
	if len(l.buckets) >= maxBuckets {
		l.buckets = map[string]*bucket{}
	}
}

// clientAddr is the connecting address without its port. aicrewd terminates
// TLS itself and runs behind no proxy, so the peer address is the client.
func clientAddr(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
