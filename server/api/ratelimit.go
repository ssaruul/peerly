package api

import (
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

type bucket struct {
	tokens   float64
	lastSeen time.Time
}

type rateLimiter struct {
	mutex   sync.Mutex
	buckets map[string]*bucket
	burst   float64
	perSec  float64
	now     func() time.Time
}

func newRateLimiter(burst int, perMinute float64) *rateLimiter {
	return &rateLimiter{buckets: map[string]*bucket{}, burst: float64(burst), perSec: perMinute / 60, now: time.Now}
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	parsed := net.ParseIP(host)
	fromProxy := parsed != nil && parsed.IsLoopback()
	if forwarded := r.Header.Get("X-Forwarded-For"); fromProxy && forwarded != "" {
		entries := strings.Split(forwarded, ",")
		return strings.TrimSpace(entries[len(entries)-1])
	}
	return host
}

func (l *rateLimiter) allow(key string) bool {
	l.mutex.Lock()
	defer l.mutex.Unlock()
	now := l.now()
	if len(l.buckets) > 10000 {
		for ip, entry := range l.buckets {
			if now.Sub(entry.lastSeen) > 10*time.Minute {
				delete(l.buckets, ip)
			}
		}
	}
	entry, found := l.buckets[key]
	if !found {
		entry = &bucket{tokens: l.burst, lastSeen: now}
		l.buckets[key] = entry
	}
	entry.tokens = min(l.burst, entry.tokens+now.Sub(entry.lastSeen).Seconds()*l.perSec)
	entry.lastSeen = now
	if entry.tokens < 1 {
		return false
	}
	entry.tokens--
	return true
}

func (l *rateLimiter) wrap(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !l.allow(clientIP(r)) {
			writeJSON(w, http.StatusTooManyRequests, map[string]string{"error": "too many attempts from this address, wait a minute and try again"})
			return
		}
		next(w, r)
	}
}
