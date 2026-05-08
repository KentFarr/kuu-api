package handlers

import (
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"golang.org/x/time/rate"
)

func mustCIDR(t *testing.T, s string) *net.IPNet {
	t.Helper()
	_, n, err := net.ParseCIDR(s)
	if err != nil {
		t.Fatalf("ParseCIDR(%q): %v", s, err)
	}
	return n
}

func TestClientIPIgnoresXFFFromUntrusted(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	r.RemoteAddr = "203.0.113.4:54321"
	r.Header.Set("X-Forwarded-For", "1.2.3.4")

	// Empty trustedProxies → never honor XFF.
	if got := clientIP(r, nil); got != "203.0.113.4" {
		t.Fatalf("with no trusted proxies, expected RemoteAddr host, got %q", got)
	}

	// Untrusted proxy CIDR → still ignore XFF.
	trusted := []*net.IPNet{mustCIDR(t, "10.0.0.0/8")}
	if got := clientIP(r, trusted); got != "203.0.113.4" {
		t.Fatalf("with untrusted RemoteAddr, expected RemoteAddr host, got %q", got)
	}
}

func TestClientIPHonorsXFFFromTrusted(t *testing.T) {
	r := httptest.NewRequest(http.MethodPost, "/", nil)
	r.RemoteAddr = "10.0.0.5:54321"
	r.Header.Set("X-Forwarded-For", "1.2.3.4, 5.6.7.8")

	trusted := []*net.IPNet{mustCIDR(t, "10.0.0.0/8")}
	if got := clientIP(r, trusted); got != "1.2.3.4" {
		t.Fatalf("with trusted proxy, expected first XFF entry, got %q", got)
	}
}

func TestRateLimiterEvictsOldestAtCap(t *testing.T) {
	l := &IPRateLimiter{
		entries:    make(map[string]*ipLimiterEntry),
		rps:        rate.Every(time.Second),
		burst:      1,
		ttl:        10 * time.Minute,
		maxEntries: 3,
	}
	for i := 0; i < 10; i++ {
		l.get(net.IPv4(192, 0, byte(i>>8), byte(i&0xff)).String())
	}
	if len(l.entries) > 3 {
		t.Fatalf("entries (%d) exceeded cap (3)", len(l.entries))
	}
}
