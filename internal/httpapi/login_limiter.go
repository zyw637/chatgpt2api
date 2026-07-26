package httpapi

import (
	"strings"
	"sync"
	"time"
)

const (
	loginIPFailureLimit       = 10
	loginIPFailureWindow      = 5 * time.Minute
	loginUserFailureLimit     = 20
	loginUserFailureWindow    = 10 * time.Minute
	loginLimiterSweepInterval = 5 * time.Minute
	loginLimiterMaxKeys       = 4096
)

type loginFailurePolicy struct {
	limit  int
	window time.Duration
}

type loginAttemptLimiter struct {
	mu         sync.Mutex
	byIP       map[string][]time.Time
	byUser     map[string][]time.Time
	ipPolicy   loginFailurePolicy
	userPolicy loginFailurePolicy
	lastSweep  time.Time
	now        func() time.Time
}

func newLoginAttemptLimiter() *loginAttemptLimiter {
	return &loginAttemptLimiter{
		byIP:       map[string][]time.Time{},
		byUser:     map[string][]time.Time{},
		ipPolicy:   loginFailurePolicy{limit: loginIPFailureLimit, window: loginIPFailureWindow},
		userPolicy: loginFailurePolicy{limit: loginUserFailureLimit, window: loginUserFailureWindow},
		now:        time.Now,
	}
}

func (l *loginAttemptLimiter) Allow(ip, username string) (bool, time.Duration) {
	if l == nil {
		return true, 0
	}
	now := l.currentTime()
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sweepLocked(now)

	ip = strings.TrimSpace(ip)
	username = strings.ToLower(strings.TrimSpace(username))
	ipFailures := pruneLoginFailures(l.byIP[ip], now.Add(-l.ipPolicy.window))
	userFailures := pruneLoginFailures(l.byUser[username], now.Add(-l.userPolicy.window))
	l.storeFailuresLocked(l.byIP, ip, ipFailures)
	l.storeFailuresLocked(l.byUser, username, userFailures)

	retryAfter := retryAfterForFailures(ipFailures, l.ipPolicy, now)
	if userRetry := retryAfterForFailures(userFailures, l.userPolicy, now); userRetry > retryAfter {
		retryAfter = userRetry
	}
	return retryAfter <= 0, retryAfter
}

func (l *loginAttemptLimiter) RecordFailure(ip, username string) {
	if l == nil {
		return
	}
	now := l.currentTime()
	ip = strings.TrimSpace(ip)
	username = strings.ToLower(strings.TrimSpace(username))
	l.mu.Lock()
	defer l.mu.Unlock()
	l.sweepLocked(now)
	if ip != "" {
		l.recordFailureLocked(l.byIP, ip, l.ipPolicy.window, now)
	}
	if username != "" {
		l.recordFailureLocked(l.byUser, username, l.userPolicy.window, now)
	}
}

func (l *loginAttemptLimiter) recordFailureLocked(target map[string][]time.Time, key string, window time.Duration, now time.Time) {
	failures := pruneLoginFailures(target[key], now.Add(-window))
	if _, exists := target[key]; !exists && len(target) >= loginLimiterMaxKeys {
		evictOldestLoginFailureKey(target)
	}
	target[key] = append(failures, now)
}

func evictOldestLoginFailureKey(target map[string][]time.Time) {
	oldestKey := ""
	var oldest time.Time
	for key, failures := range target {
		if len(failures) == 0 {
			delete(target, key)
			return
		}
		last := failures[len(failures)-1]
		if oldestKey == "" || last.Before(oldest) {
			oldestKey = key
			oldest = last
		}
	}
	if oldestKey != "" {
		delete(target, oldestKey)
	}
}

func (l *loginAttemptLimiter) Reset(ip, username string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	delete(l.byIP, strings.TrimSpace(ip))
	delete(l.byUser, strings.ToLower(strings.TrimSpace(username)))
}

func (l *loginAttemptLimiter) currentTime() time.Time {
	if l.now != nil {
		return l.now().UTC()
	}
	return time.Now().UTC()
}

func (l *loginAttemptLimiter) sweepLocked(now time.Time) {
	if !l.lastSweep.IsZero() && now.Sub(l.lastSweep) < loginLimiterSweepInterval {
		return
	}
	for key, failures := range l.byIP {
		l.storeFailuresLocked(l.byIP, key, pruneLoginFailures(failures, now.Add(-l.ipPolicy.window)))
	}
	for key, failures := range l.byUser {
		l.storeFailuresLocked(l.byUser, key, pruneLoginFailures(failures, now.Add(-l.userPolicy.window)))
	}
	l.lastSweep = now
}

func (l *loginAttemptLimiter) storeFailuresLocked(target map[string][]time.Time, key string, failures []time.Time) {
	if key == "" || len(failures) == 0 {
		delete(target, key)
		return
	}
	target[key] = failures
}

func pruneLoginFailures(failures []time.Time, cutoff time.Time) []time.Time {
	first := 0
	for first < len(failures) && failures[first].Before(cutoff) {
		first++
	}
	return failures[first:]
}

func retryAfterForFailures(failures []time.Time, policy loginFailurePolicy, now time.Time) time.Duration {
	if policy.limit <= 0 || len(failures) < policy.limit {
		return 0
	}
	retryAfter := failures[len(failures)-policy.limit].Add(policy.window).Sub(now)
	if retryAfter < time.Second {
		return time.Second
	}
	return retryAfter
}
