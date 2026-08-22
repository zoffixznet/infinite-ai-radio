package accounts

import (
	"sync"
	"time"
)

// Limiter throttles failed login attempts per key (an address or an
// account): once a key accumulates max failures inside the window, it is
// refused until the oldest failure ages out. Successes clear the key.
type Limiter struct {
	max    int
	window time.Duration

	mu    sync.Mutex
	fails map[string][]time.Time
	now   func() time.Time
}

// NewLimiter returns a limiter allowing max failures per window.
func NewLimiter(max int, window time.Duration) *Limiter {
	return &Limiter{max: max, window: window, fails: map[string][]time.Time{}, now: time.Now}
}

// pruneLocked drops failures older than the window for key.
func (l *Limiter) pruneLocked(key string, now time.Time) []time.Time {
	list := l.fails[key]
	cut := 0
	for cut < len(list) && now.Sub(list[cut]) >= l.window {
		cut++
	}
	list = list[cut:]
	if len(list) == 0 {
		delete(l.fails, key)
	} else {
		l.fails[key] = list
	}
	return list
}

// Allowed reports whether key may attempt a login now; when refused, it
// also says how long until the next attempt is accepted.
func (l *Limiter) Allowed(key string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	list := l.pruneLocked(key, now)
	if len(list) < l.max {
		return true, 0
	}
	return false, l.window - now.Sub(list[0])
}

// Fail records a failed attempt for key.
func (l *Limiter) Fail(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.now()
	l.pruneLocked(key, now)
	l.fails[key] = append(l.fails[key], now)
}

// Reset clears key after a successful login.
func (l *Limiter) Reset(key string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.fails, key)
}
