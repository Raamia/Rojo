package api

import (
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

type tokenBucket struct {
	capacity   int
	refillRate float64
	tokens     float64
	last       time.Time
	// lastSeen is when this client last made a request — the eviction signal.
	// Distinct from `last`, which is the refill bookkeeping timestamp.
	lastSeen time.Time
}

func newBucket(capacity int, refillPerSec float64) *tokenBucket {
	now := time.Now()
	return &tokenBucket{
		capacity:   capacity,
		refillRate: refillPerSec,
		tokens:     float64(capacity),
		last:       now,
		lastSeen:   now,
	}
}

func (b *tokenBucket) allow() bool {
	now := time.Now()
	elapsed := now.Sub(b.last).Seconds()
	b.last = now
	b.lastSeen = now
	b.tokens += elapsed * b.refillRate
	if b.tokens > float64(b.capacity) {
		b.tokens = float64(b.capacity)
	}
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// Eviction bounds the bucket map, which otherwise grows by one entry per
// distinct client IP for the life of the process — an unbounded, monotonic
// leak fed by whoever connects.
//
// A bucket idle longer than bucketIdleTTL holds a full allowance anyway, so
// dropping it changes nothing about what its owner may do next; the sweep only
// forgets clients whose state has stopped mattering. Sweeping inline every
// sweepEvery inserts — rather than from a janitor goroutine — keeps the
// limiter free of anything that needs a shutdown plan.
const (
	bucketIdleTTL = 10 * time.Minute
	sweepEvery    = 256
)

type RateLimiter struct {
	mu       sync.Mutex
	buckets  map[string]*tokenBucket
	capacity int
	refill   float64

	// trustProxyHeader controls whether X-Forwarded-For decides a client's
	// identity. Off by default: XFF is written by whoever connects, so
	// honouring it from an untrusted peer lets any client mint a fresh
	// identity — and a fresh, full bucket — per request, which is no limiter
	// at all. Only a deployment actually behind a proxy it controls should
	// turn this on.
	trustProxyHeader bool

	inserts int // new buckets since the last sweep
}

func NewRateLimiter(capacity int, refillPerSec float64) *RateLimiter {
	return &RateLimiter{
		buckets:  make(map[string]*tokenBucket),
		capacity: capacity,
		refill:   refillPerSec,
	}
}

// TrustProxyHeader opts in to keying clients by X-Forwarded-For. Call it only
// when a trusted reverse proxy is what connects to this server — the header is
// then written by the proxy, not the client, and names the real caller.
func (rl *RateLimiter) TrustProxyHeader() *RateLimiter {
	rl.trustProxyHeader = true
	return rl
}

func (rl *RateLimiter) Middleware() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// Liveness/readiness probes must never be rate limited. They share
			// a source IP with ordinary traffic (or with each other behind a
			// NAT/ingress), so under exactly the load spike where the process
			// needs to stay up, probes would start getting 429s and the
			// orchestrator would restart a healthy pod — shedding capacity and
			// pushing the spike onto the remaining ones.
			if r.URL.Path == healthPath {
				next.ServeHTTP(w, r)
				return
			}
			key := rl.clientKey(r)
			rl.mu.Lock()
			b, ok := rl.buckets[key]
			if !ok {
				b = newBucket(rl.capacity, rl.refill)
				rl.buckets[key] = b
				rl.maybeSweepLocked()
			}
			allowed := b.allow()
			rl.mu.Unlock()
			if !allowed {
				WriteJSONError(w, http.StatusTooManyRequests, "rate limit exceeded")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// maybeSweepLocked drops idle buckets, amortised across inserts so the cost
// stays O(1) per request. The caller must hold rl.mu.
func (rl *RateLimiter) maybeSweepLocked() {
	rl.inserts++
	if rl.inserts < sweepEvery {
		return
	}
	rl.inserts = 0
	cutoff := time.Now().Add(-bucketIdleTTL)
	for key, b := range rl.buckets {
		if b.lastSeen.Before(cutoff) {
			delete(rl.buckets, key)
		}
	}
}

func (rl *RateLimiter) clientKey(r *http.Request) string {
	// The connection's own address is the only identity the peer cannot
	// choose. X-Forwarded-For is just a header: unless a trusted proxy is
	// known to be the one setting it, believing it hands every client an
	// unlimited supply of fresh identities.
	if rl.trustProxyHeader {
		if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
			// XFF is "client, proxy1, proxy2"; the originating client is the
			// first entry.
			if i := strings.IndexByte(fwd, ','); i >= 0 {
				fwd = fwd[:i]
			}
			return strings.TrimSpace(fwd)
		}
	}
	// RemoteAddr is "IP:port". Key on the IP alone so all connections from
	// one client share a bucket — otherwise every new TCP connection (a fresh
	// ephemeral port) gets its own full bucket and the limit is bypassable.
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}
