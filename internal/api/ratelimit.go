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
}

func newBucket(capacity int, refillPerSec float64) *tokenBucket {
	return &tokenBucket{
		capacity:   capacity,
		refillRate: refillPerSec,
		tokens:     float64(capacity),
		last:       time.Now(),
	}
}

func (b *tokenBucket) allow() bool {
	now := time.Now()
	elapsed := now.Sub(b.last).Seconds()
	b.last = now
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

type RateLimiter struct {
	mu       sync.Mutex
	buckets  map[string]*tokenBucket
	capacity int
	refill   float64
}

func NewRateLimiter(capacity int, refillPerSec float64) *RateLimiter {
	return &RateLimiter{
		buckets:  make(map[string]*tokenBucket),
		capacity: capacity,
		refill:   refillPerSec,
	}
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
			key := clientKey(r)
			rl.mu.Lock()
			b, ok := rl.buckets[key]
			if !ok {
				b = newBucket(rl.capacity, rl.refill)
				rl.buckets[key] = b
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

func clientKey(r *http.Request) string {
	if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
		// XFF is a comma-separated list "client, proxy1, proxy2"; the
		// originating client is the first entry.
		if i := strings.IndexByte(fwd, ','); i >= 0 {
			fwd = fwd[:i]
		}
		return strings.TrimSpace(fwd)
	}
	// RemoteAddr is "IP:port". Key on the IP alone so all connections from
	// one client share a bucket — otherwise every new TCP connection (a fresh
	// ephemeral port) gets its own full bucket and the limit is bypassable.
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}
