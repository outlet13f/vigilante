package api

import (
	"fmt"
	"math"
	"net/http"
	"strconv"
	"sync"
	"time"

	"vigilante/internal/auth"
	"vigilante/internal/config"
)

// Rate classes: emergency calls (rollback, approval, abort, circuit) draw
// from their own bucket so that a flood of reads cannot block them.
const (
	classDefault   = "default"
	classEmergency = "emergency"
)

type bucket struct {
	tokens float64
	last   time.Time
	day    string
	used   int
}

type limiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
	now     func() time.Time
}

func newLimiter() *limiter {
	return &limiter{buckets: map[string]*bucket{}, now: time.Now}
}

type verdict struct {
	ok         bool
	limit      int
	remaining  int
	retryAfter time.Duration
	why        string
}

// take spends one request from key's bucket.
func (l *limiter) take(key string, rl config.RateLimit) verdict {
	if rl.Rate <= 0 && rl.Daily <= 0 {
		return verdict{ok: true, limit: -1}
	}
	now := l.now()
	day := now.UTC().Format("2006-01-02")
	l.mu.Lock()
	defer l.mu.Unlock()
	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{tokens: float64(rl.Burst), last: now, day: day}
		l.buckets[key] = b
	}
	if b.day != day {
		b.day, b.used = day, 0
	}
	v := verdict{ok: true, limit: rl.Burst}
	if rl.Rate > 0 {
		b.tokens = math.Min(float64(rl.Burst), b.tokens+now.Sub(b.last).Seconds()*rl.Rate)
		b.last = now
		if b.tokens < 1 {
			v.ok = false
			v.retryAfter = time.Duration((1 - b.tokens) / rl.Rate * float64(time.Second))
			v.why = fmt.Sprintf("more than %g requests per second (burst %d)", rl.Rate, rl.Burst)
		}
	}
	if v.ok && rl.Daily > 0 && b.used >= rl.Daily {
		midnight := now.UTC().Truncate(24 * time.Hour).Add(24 * time.Hour)
		v.ok, v.retryAfter, v.why = false, midnight.Sub(now), fmt.Sprintf("daily quota of %d requests used", rl.Daily)
	}
	if v.ok {
		if rl.Rate > 0 {
			b.tokens--
		}
		b.used++
	}
	v.remaining = int(math.Max(0, math.Floor(b.tokens)))
	if rl.Rate <= 0 {
		v.limit, v.remaining = rl.Daily, rl.Daily-b.used
	}
	// Forget idle buckets now and then.
	if len(l.buckets) > 10000 {
		for k, ob := range l.buckets {
			if now.Sub(ob.last) > time.Hour {
				delete(l.buckets, k)
			}
		}
	}
	return v
}

// limitFor returns the bucket settings for a caller and class. An API
// client may carry its own default-class limit.
func (s *Server) limitFor(p *auth.Principal, class string) config.RateLimit {
	if class == classEmergency {
		return *s.E.Cfg.API.EmergencyRateLimit
	}
	if p.ClientID != "" {
		if c, ok := s.E.Client(p.ClientID); ok && c.RateLimit != nil {
			return config.RateLimit{Rate: c.RateLimit.Rate, Burst: c.RateLimit.Burst, Daily: c.RateLimit.Daily}
		}
	}
	return *s.E.Cfg.API.RateLimit
}

// limited applies the caller's rate limit. The legacy break-glass token is
// exempt: it exists for emergencies.
func (s *Server) limited(class string, next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p := auth.FromContext(r.Context())
		if p == nil || p.Kind == "legacy" {
			next(w, r)
			return
		}
		v := s.limits.take(class+"|"+p.ID, s.limitFor(p, class))
		if v.limit >= 0 {
			w.Header().Set("RateLimit-Limit", strconv.Itoa(v.limit))
			w.Header().Set("RateLimit-Remaining", strconv.Itoa(v.remaining))
		}
		if !v.ok {
			secs := int(math.Ceil(v.retryAfter.Seconds()))
			w.Header().Set("Retry-After", strconv.Itoa(max(secs, 1)))
			w.Header().Set("RateLimit-Reset", strconv.Itoa(max(secs, 1)))
			s.problem(w, r, http.StatusTooManyRequests, "rate_limited", fmt.Sprintf("rate limit (%s bucket): %s", class, v.why))
			return
		}
		next(w, r)
	}
}
