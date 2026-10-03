package server

import (
	"math"
	"net"
	"net/http"
	"net/netip"
	"sync"
	"time"
)

// limiter is an in-memory token bucket per key: at most rate requests in a
// burst, refilled at rate per window. Limits reset when the process
// restarts, which is acceptable for bounding abuse of one aicrewd.
//
// The table holds at most max buckets. To admit a new key into a full table
// it evicts only buckets that have refilled completely: a full bucket holds
// no state, since a new one starts full, so the eviction loses nothing. A
// bucket still draining is never evicted, and when every bucket is still
// draining a new key is refused until one has refilled. A flood of new keys
// therefore never restores anyone's budget; it can only lock new keys out
// for at most one window.
type limiter struct {
	mu      sync.Mutex
	rate    float64
	window  time.Duration
	max     int
	now     func() time.Time
	buckets map[string]*bucket
	// fullAt is when the earliest bucket will have refilled, as of the last
	// sweep of a full table; until then a new key is refused without
	// sweeping again.
	fullAt time.Time
	// sweeps counts sweeps of a full table.
	sweeps int
}

type bucket struct {
	tokens float64
	last   time.Time
}

// maxBuckets bounds the memory a limiter uses.
const maxBuckets = 4096

func newLimiter(rate int, window time.Duration) *limiter {
	return &limiter{rate: float64(rate), window: window, max: maxBuckets, now: time.Now,
		buckets: map[string]*bucket{}}
}

// allow takes one token for key. When none is left, or key is new and the
// table has no room, it returns false and how long until a token is likely.
func (l *limiter) allow(key string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	b, ok := l.buckets[key]
	if !ok {
		if len(l.buckets) >= l.max && !l.makeRoom(now) {
			return false, l.fullAt.Sub(now)
		}
		b = &bucket{tokens: l.rate, last: now}
		l.buckets[key] = b
	}
	l.refill(b, now)
	if b.tokens < 1 {
		return false, l.untilTokens(b, 1)
	}
	b.tokens--
	return true, 0
}

// spent reports, without taking a token, whether key has none left, and how
// long until it likely has one. A key without a bucket has its full budget.
func (l *limiter) spent(key string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	b, ok := l.buckets[key]
	if !ok {
		return false, 0
	}
	l.refill(b, l.now())
	if b.tokens < 1 {
		return true, l.untilTokens(b, 1)
	}
	return false, 0
}

func (l *limiter) refill(b *bucket, now time.Time) {
	if elapsed := now.Sub(b.last); elapsed > 0 {
		b.tokens = math.Min(l.rate, b.tokens+elapsed.Seconds()*l.rate/l.window.Seconds())
		b.last = now
	}
}

// untilTokens is how long b takes to hold n tokens.
func (l *limiter) untilTokens(b *bucket, n float64) time.Duration {
	if b.tokens >= n {
		return 0
	}
	return time.Duration(math.Ceil((n - b.tokens) * float64(l.window) / l.rate))
}

// makeRoom evicts every bucket that has refilled completely and reports
// whether any was. When none has, it records when the first will, and
// refuses without sweeping until then.
func (l *limiter) makeRoom(now time.Time) bool {
	if now.Before(l.fullAt) {
		return false
	}
	l.sweeps++
	earliest := time.Duration(math.MaxInt64)
	for k, b := range l.buckets {
		l.refill(b, now)
		if b.tokens >= l.rate {
			delete(l.buckets, k)
			continue
		}
		earliest = min(earliest, l.untilTokens(b, l.rate))
	}
	if len(l.buckets) < l.max {
		return true
	}
	l.fullAt = now.Add(earliest)
	return false
}

// clientAddr is the key of the connecting client. aicrewd terminates TLS
// itself and runs behind no proxy, so the peer address is the client. An
// IPv4 client, including one seen as IPv4-mapped IPv6, is keyed by its
// address. An IPv6 client is keyed by its /64: one subscriber or host holds
// at least a /64, so cycling addresses inside it gains nothing.
func clientAddr(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return host
	}
	addr = addr.Unmap().WithZone("")
	if addr.Is4() {
		return addr.String()
	}
	return netip.PrefixFrom(addr, 64).Masked().String()
}
