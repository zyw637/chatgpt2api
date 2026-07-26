package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestLoginEndpointReturnsTooManyRequestsAfterFailures(t *testing.T) {
	app := newTestApp(t)
	defer app.Close()
	app.loginLimit.ipPolicy = loginFailurePolicy{limit: 1, window: time.Minute}
	app.loginLimit.userPolicy = loginFailurePolicy{limit: 100, window: time.Minute}

	request := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, "/auth/login", strings.NewReader(`{"username":"missing","password":"wrong-password"}`))
		req.RemoteAddr = "192.0.2.25:1234"
		res := httptest.NewRecorder()
		app.Handler().ServeHTTP(res, req)
		return res
	}
	if res := request(); res.Code != http.StatusBadRequest {
		t.Fatalf("first login status = %d body = %s", res.Code, res.Body.String())
	}
	res := request()
	if res.Code != http.StatusTooManyRequests {
		t.Fatalf("limited login status = %d body = %s", res.Code, res.Body.String())
	}
	if res.Header().Get("Retry-After") == "" {
		t.Fatal("limited login response is missing Retry-After")
	}
}

func TestLoginAttemptLimiterBlocksAndResetsFailures(t *testing.T) {
	now := time.Date(2026, time.July, 18, 12, 0, 0, 0, time.UTC)
	limiter := newLoginAttemptLimiter()
	limiter.ipPolicy = loginFailurePolicy{limit: 2, window: time.Minute}
	limiter.userPolicy = loginFailurePolicy{limit: 3, window: 2 * time.Minute}
	limiter.now = func() time.Time { return now }

	limiter.RecordFailure("192.0.2.1", "Alice")
	if allowed, _ := limiter.Allow("192.0.2.1", "alice"); !allowed {
		t.Fatal("single failure should not block login")
	}
	limiter.RecordFailure("192.0.2.1", "alice")
	if allowed, retryAfter := limiter.Allow("192.0.2.1", "alice"); allowed || retryAfter != time.Minute {
		t.Fatalf("Allow() = %v, %v; want false, 1m", allowed, retryAfter)
	}

	limiter.Reset("192.0.2.1", "ALICE")
	if allowed, _ := limiter.Allow("192.0.2.1", "alice"); !allowed {
		t.Fatal("successful login reset did not clear failures")
	}

	limiter.RecordFailure("192.0.2.1", "alice")
	limiter.RecordFailure("192.0.2.1", "alice")
	now = now.Add(time.Minute + time.Second)
	if allowed, _ := limiter.Allow("192.0.2.1", "alice"); !allowed {
		t.Fatal("expired IP failures still blocked login")
	}
}

func TestClientIPIgnoresForwardedHeadersFromUntrustedPeer(t *testing.T) {
	t.Setenv("TRUSTED_PROXY_CIDRS", "")
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "198.51.100.20:1234"
	req.Header.Set("X-Forwarded-For", "203.0.113.8")
	req.Header.Set("X-Real-IP", "203.0.113.9")
	if got := clientIP(req); got != "198.51.100.20" {
		t.Fatalf("clientIP() = %q, want direct peer", got)
	}

	req.RemoteAddr = "127.0.0.1:1234"
	if got := clientIP(req); got != "203.0.113.8" {
		t.Fatalf("clientIP() trusted proxy = %q, want forwarded address", got)
	}
}

func TestClientIPWalksTrustedProxyChainFromRight(t *testing.T) {
	t.Setenv("TRUSTED_PROXY_CIDRS", "10.0.0.0/8")
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "10.0.0.2:1234"
	req.Header.Set("X-Forwarded-For", "203.0.113.99, 198.51.100.25, 10.0.0.3")
	if got := clientIP(req); got != "198.51.100.25" {
		t.Fatalf("clientIP() = %q, want first untrusted address from the right", got)
	}
}

func TestClientIPIgnoresDockerProxyUnlessExplicitlyTrusted(t *testing.T) {
	t.Setenv("TRUSTED_PROXY_CIDRS", "")
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = "172.18.0.4:1234"
	req.Header.Set("X-Forwarded-For", "198.51.100.25")
	if got := clientIP(req); got != "172.18.0.4" {
		t.Fatalf("clientIP() = %q, want direct untrusted peer", got)
	}

	t.Setenv("TRUSTED_PROXY_CIDRS", "172.18.0.0/16")
	if got := clientIP(req); got != "198.51.100.25" {
		t.Fatalf("clientIP() configured proxy = %q, want forwarded client", got)
	}
}

func TestLoginAttemptLimiterCapsTrackedKeys(t *testing.T) {
	limiter := newLoginAttemptLimiter()
	for i := 0; i < loginLimiterMaxKeys+100; i++ {
		limiter.RecordFailure("192.0.2."+strconv.Itoa(i%256)+":"+strconv.Itoa(i), "user-"+strconv.Itoa(i))
	}
	if len(limiter.byIP) > loginLimiterMaxKeys || len(limiter.byUser) > loginLimiterMaxKeys {
		t.Fatalf("tracked keys exceeded limit: ip=%d user=%d", len(limiter.byIP), len(limiter.byUser))
	}
}
