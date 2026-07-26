package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestAppRejectsOversizedRequestBodiesBeforeRouting(t *testing.T) {
	for _, tc := range []struct {
		name        string
		path        string
		contentType string
		limit       int64
	}{
		{name: "json", path: "/api/test", contentType: "application/json", limit: maxJSONRequestBodyBytes},
		{name: "image multipart", path: "/v1/images/edits", contentType: "multipart/form-data; boundary=test", limit: maxMultipartRequestBodyBytes},
		{name: "external multipart", path: "/api/external-image-tasks/generations", contentType: "multipart/form-data; boundary=test", limit: maxExternalMultipartBytes},
	} {
		t.Run(tc.name, func(t *testing.T) {
			called := false
			app := &App{}
			routes := []appRoute{exact(http.MethodPost, tc.path, func(w http.ResponseWriter, _ *http.Request) {
				called = true
				w.WriteHeader(http.StatusNoContent)
			})}
			req := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader("x"))
			req.Header.Set("Content-Type", tc.contentType)
			req.ContentLength = tc.limit + 1
			res := httptest.NewRecorder()

			app.serveObservedHTTP(res, req, routes)

			if res.Code != http.StatusRequestEntityTooLarge {
				t.Fatalf("status = %d body = %s", res.Code, res.Body.String())
			}
			if called {
				t.Fatal("oversized request reached route handler")
			}
		})
	}
}

func TestAppRejectsChunkedJSONBodyOverLimit(t *testing.T) {
	body := `{"value":"` + strings.Repeat("a", maxJSONRequestBodyBytes) + `"}`
	app := &App{}
	routes := []appRoute{exact(http.MethodPost, "/api/test", func(w http.ResponseWriter, r *http.Request) {
		_, err := readJSONMap(r)
		if err != nil {
			writeRequestBodyError(w, err, "invalid json body")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})}
	req := httptest.NewRequest(http.MethodPost, "/api/test", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.ContentLength = -1
	res := httptest.NewRecorder()

	app.serveObservedHTTP(res, req, routes)

	if res.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d body = %s", res.Code, res.Body.String())
	}
}

func TestAppRejectsChunkedJSONBodyWithOversizedTrailingData(t *testing.T) {
	body := `{}` + strings.Repeat(" ", maxJSONRequestBodyBytes)
	app := &App{}
	routes := []appRoute{exact(http.MethodPost, "/api/test", func(w http.ResponseWriter, r *http.Request) {
		if _, err := readJSONMap(r); err != nil {
			writeRequestBodyError(w, err, "invalid json body")
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})}
	req := httptest.NewRequest(http.MethodPost, "/api/test", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.ContentLength = -1
	res := httptest.NewRecorder()

	app.serveObservedHTTP(res, req, routes)

	if res.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d body = %s", res.Code, res.Body.String())
	}
}

func TestMatchAppRoute(t *testing.T) {
	routes := []appRoute{
		exact(http.MethodGet, "/version", nil),
		exact("", "/api/settings", nil),
		subtree("/api/auth/users", nil),
		prefix("/images/", nil),
	}

	for _, tc := range []struct {
		name   string
		method string
		path   string
		want   string
	}{
		{name: "exact method", method: http.MethodGet, path: "/version", want: "/version"},
		{name: "exact method mismatch", method: http.MethodPost, path: "/version", want: ""},
		{name: "methodless exact", method: http.MethodPost, path: "/api/settings", want: "/api/settings"},
		{name: "subtree base", method: http.MethodGet, path: "/api/auth/users", want: "/api/auth/users"},
		{name: "subtree child", method: http.MethodGet, path: "/api/auth/users/123/key", want: "/api/auth/users"},
		{name: "subtree boundary", method: http.MethodGet, path: "/api/auth/users123", want: ""},
		{name: "static prefix", method: http.MethodHead, path: "/images/2026/04/a.png", want: "/images/"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			route := matchAppRoute(routes, tc.method, tc.path)
			got := ""
			if route != nil {
				got = route.path
			}
			if got != tc.want {
				t.Fatalf("matchAppRoute(%q, %q) = %q, want %q", tc.method, tc.path, got, tc.want)
			}
		})
	}
}

func TestAppRouterKeepsAPIMissesOutOfSPA(t *testing.T) {
	app := newTestApp(t)
	defer app.Close()

	req := httptest.NewRequest(http.MethodGet, "/api/missing", nil)
	res := httptest.NewRecorder()
	app.Handler().ServeHTTP(res, req)
	if res.Code != http.StatusNotFound {
		t.Fatalf("missing API status = %d body = %s", res.Code, res.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/settings", nil)
	res = httptest.NewRecorder()
	app.Handler().ServeHTTP(res, req)
	if res.Code != http.StatusOK || !strings.Contains(res.Body.String(), `<div id="root">`) || !strings.Contains(res.Body.String(), `id="pwa-boot"`) {
		t.Fatalf("SPA route status/body = %d %q", res.Code, res.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/auth/linuxdo/callback", nil)
	res = httptest.NewRecorder()
	app.Handler().ServeHTTP(res, req)
	if res.Code != http.StatusOK || !strings.Contains(res.Body.String(), `<div id="root">`) || !strings.Contains(res.Body.String(), `id="pwa-boot"`) {
		t.Fatalf("Linuxdo frontend callback status/body = %d %q", res.Code, res.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/auth/missing", nil)
	res = httptest.NewRecorder()
	app.Handler().ServeHTTP(res, req)
	if res.Code != http.StatusNotFound {
		t.Fatalf("missing auth API status = %d body = %s", res.Code, res.Body.String())
	}
}
