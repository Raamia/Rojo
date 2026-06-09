package api

import (
	"net/http"
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
		return fwd
	}
	return r.RemoteAddr
}
