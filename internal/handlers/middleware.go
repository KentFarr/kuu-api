package handlers

import (
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"

	"github.com/KentFarr/kuu-api/internal/logging"
	"github.com/KentFarr/kuu-api/internal/metrics"
)

const requestIDHeader = "X-Request-ID"

// RequestIDMiddleware honors an inbound X-Request-ID when supplied (so a
// reverse proxy or front-end can stitch together logs across services),
// otherwise mints one. The ID is bound to the request context and echoed
// back to the client so support can correlate "I made this request" with
// log lines after the fact.
func RequestIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimSpace(r.Header.Get(requestIDHeader))
		if id == "" || len(id) > 64 {
			id = logging.NewRequestID()
		}
		w.Header().Set(requestIDHeader, id)
		ctx := logging.WithRequestID(r.Context(), id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func CORSMiddleware(allowedOrigin string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Vary", "Origin")
			w.Header().Set("Access-Control-Allow-Origin", allowedOrigin)
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type")

			if r.Method == http.MethodOptions {
				w.WriteHeader(http.StatusOK)
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

func SecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}

type ipLimiterEntry struct {
	limiter *rate.Limiter
	seen    time.Time
}

const defaultRateLimiterMaxEntries = 10000

type IPRateLimiter struct {
	mu             sync.Mutex
	entries        map[string]*ipLimiterEntry
	rps            rate.Limit
	burst          int
	ttl            time.Duration
	maxEntries     int
	trustedProxies []*net.IPNet
}

func NewIPRateLimiter(rps rate.Limit, burst int, trustedProxies []*net.IPNet) *IPRateLimiter {
	l := &IPRateLimiter{
		entries:        make(map[string]*ipLimiterEntry),
		rps:            rps,
		burst:          burst,
		ttl:            10 * time.Minute,
		maxEntries:     defaultRateLimiterMaxEntries,
		trustedProxies: trustedProxies,
	}
	go l.cleanup()
	return l
}

func (l *IPRateLimiter) cleanup() {
	ticker := time.NewTicker(5 * time.Minute)
	defer ticker.Stop()
	for range ticker.C {
		cutoff := time.Now().Add(-l.ttl)
		l.mu.Lock()
		for ip, e := range l.entries {
			if e.seen.Before(cutoff) {
				delete(l.entries, ip)
			}
		}
		size := len(l.entries)
		l.mu.Unlock()
		metrics.RateLimiterEntries.Set(float64(size))
	}
}

func (l *IPRateLimiter) get(ip string) *rate.Limiter {
	l.mu.Lock()
	e, ok := l.entries[ip]
	if !ok {
		if len(l.entries) >= l.maxEntries {
			l.evictOldestLocked()
		}
		e = &ipLimiterEntry{limiter: rate.NewLimiter(l.rps, l.burst)}
		l.entries[ip] = e
	}
	e.seen = time.Now()
	size := len(l.entries)
	l.mu.Unlock()
	metrics.RateLimiterEntries.Set(float64(size))
	return e.limiter
}

func (l *IPRateLimiter) evictOldestLocked() {
	var oldestIP string
	var oldestTime time.Time
	for ip, e := range l.entries {
		if oldestIP == "" || e.seen.Before(oldestTime) {
			oldestIP = ip
			oldestTime = e.seen
		}
	}
	if oldestIP != "" {
		delete(l.entries, oldestIP)
	}
}

func (l *IPRateLimiter) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip := clientIP(r, l.trustedProxies)
		if !l.get(ip).Allow() {
			http.Error(w, "rate limit exceeded", http.StatusTooManyRequests)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func clientIP(r *http.Request, trustedProxies []*net.IPNet) string {
	remote := remoteHost(r)
	if len(trustedProxies) > 0 && remoteIsTrusted(remote, trustedProxies) {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			if i := strings.IndexByte(xff, ','); i >= 0 {
				return strings.TrimSpace(xff[:i])
			}
			return strings.TrimSpace(xff)
		}
	}
	return remote
}

func remoteHost(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func remoteIsTrusted(host string, trustedProxies []*net.IPNet) bool {
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	for _, n := range trustedProxies {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}
