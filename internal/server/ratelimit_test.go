package server

import (
	"fmt"
	"net/http"
	"testing"
	"time"
)

func fixedLimiter(rate int) (*limiter, *time.Time) {
	now := time.Unix(1_000_000, 0)
	l := newLimiter(rate, time.Minute)
	l.now = func() time.Time { return now }
	return l, &now
}

func exhaust(t *testing.T, l *limiter, key string) {
	t.Helper()
	for {
		if ok, _ := l.allow(key); !ok {
			return
		}
	}
}

// A flood of distinct addresses never gives an exhausted client a fresh
// budget: once the table is full of draining buckets, new addresses are
// refused and nothing is reset. After a window, everyone is served again.
func TestLimiterFloodDoesNotReset(t *testing.T) {
	l, now := fixedLimiter(10)
	exhaust(t, l, "victim")
	for i := 0; i < 3*maxBuckets; i++ {
		ok, wait := l.allow(fmt.Sprintf("flood-%d", i))
		if full := i >= maxBuckets-1; ok == full {
			t.Fatalf("flood key %d: allowed=%v with %d buckets", i, ok, len(l.buckets))
		} else if !ok && (wait <= 0 || wait > time.Minute) {
			t.Fatalf("flood key %d: Retry-After %v", i, wait)
		}
		if len(l.buckets) > maxBuckets {
			t.Fatalf("%d buckets, bound %d", len(l.buckets), maxBuckets)
		}
	}
	if ok, _ := l.allow("victim"); ok {
		t.Fatal("the flood restored the exhausted client's budget")
	}
	*now = now.Add(time.Minute)
	if ok, _ := l.allow("victim"); !ok {
		t.Fatal("the client was not served after a window")
	}
	if ok, _ := l.allow("newcomer"); !ok {
		t.Fatal("a new client was not served once buckets had refilled")
	}
	if len(l.buckets) > maxBuckets {
		t.Fatalf("%d buckets", len(l.buckets))
	}
}

// A full table refuses a new key while every bucket drains, then evicts only
// the buckets that have refilled; a partly drained bucket is never evicted.
func TestLimiterEvictsOnlyRefilledBuckets(t *testing.T) {
	l, now := fixedLimiter(10)
	l.max = 3
	l.allow("a")       // 9 left
	exhaust(t, l, "b") // 0 left
	l.allow("c")       // 9 left
	if ok, _ := l.allow("d"); ok {
		t.Fatal("a full table of draining buckets admitted a new key")
	}
	if len(l.buckets) != 3 {
		t.Fatalf("%d buckets", len(l.buckets))
	}
	*now = now.Add(6 * time.Second) // a and c refill; b holds 1
	if ok, _ := l.allow("d"); !ok {
		t.Fatal("refilled buckets were not evicted for a new key")
	}
	if _, kept := l.buckets["b"]; !kept {
		t.Fatal("a draining bucket was evicted")
	}
	if ok, _ := l.allow("b"); !ok {
		t.Fatal("b lost its refilled token")
	}
	if ok, _ := l.allow("b"); ok {
		t.Fatal("b got a fresh budget")
	}
}

// The refusal of a full table does not sweep again before a bucket can
// have refilled.
func TestLimiterRememberWhenFull(t *testing.T) {
	l, now := fixedLimiter(10)
	l.max = 2
	exhaust(t, l, "a")
	l.allow("b")
	ok, wait := l.allow("c")
	if ok || wait != 6*time.Second {
		t.Fatalf("full table: %v, Retry-After %v; want refused for 6 s (b's refill)", ok, wait)
	}
	if l.fullAt != now.Add(6*time.Second) {
		t.Fatalf("fullAt = %v", l.fullAt)
	}
	for i := 0; i < 1000; i++ {
		l.allow(fmt.Sprintf("flood-%d", i))
	}
	if l.sweeps != 1 {
		t.Fatalf("%d sweeps for one refilling interval, want 1", l.sweeps)
	}
	*now = now.Add(6 * time.Second)
	if ok, _ := l.allow("c"); !ok {
		t.Fatal("not admitted once b refilled")
	}
}

func TestClientAddrKeys(t *testing.T) {
	key := func(remote string) string {
		return clientAddr(&http.Request{RemoteAddr: remote})
	}
	cases := map[string]string{
		"192.0.2.7:443":                  "192.0.2.7",
		"[::ffff:192.0.2.7]:443":         "192.0.2.7",
		"[2001:db8:1:2:aaaa::1]:443":     "2001:db8:1:2::/64",
		"[2001:db8:1:2:ffff:ffff:0:9]:1": "2001:db8:1:2::/64",
		"[2001:db8:1:3::1]:443":          "2001:db8:1:3::/64",
		"[fe80::1%eth0]:443":             "fe80::/64",
		"unix":                           "unix",
	}
	for remote, want := range cases {
		if got := key(remote); got != want {
			t.Errorf("%s: key %q, want %q", remote, got, want)
		}
	}
}

// refund gives back a token allow took, never beyond the full burst, and
// does nothing for a key without a bucket.
func TestLimiterRefund(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	l := newLimiter(2, time.Minute)
	l.now = func() time.Time { return now }
	l.refund("a")
	if _, ok := l.buckets["a"]; ok {
		t.Fatal("refund created a bucket")
	}
	l.allow("a")
	l.allow("a")
	if ok, _ := l.allow("a"); ok {
		t.Fatal("a third token within the burst")
	}
	l.refund("a")
	if ok, _ := l.allow("a"); !ok {
		t.Fatal("the refunded token is not there")
	}
	l.refund("a")
	l.refund("a")
	l.refund("a")
	if b := l.buckets["a"]; b.tokens != 2 {
		t.Fatalf("refunds filled the bucket to %v, beyond the burst of 2", b.tokens)
	}
}
