package web

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHandlerServesEmbeddedSPA(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/settings", nil)
	res := httptest.NewRecorder()
	Handler().ServeHTTP(res, req)
	if res.Code != http.StatusOK {
		t.Fatalf("SPA route status = %d body = %s", res.Code, res.Body.String())
	}
	if !strings.Contains(res.Body.String(), `<div id="root">`) {
		t.Fatalf("SPA route body missing root element: %q", res.Body.String())
	}
	if !strings.Contains(res.Body.String(), `id="pwa-boot"`) {
		t.Fatalf("SPA route body missing boot screen: %q", res.Body.String())
	}
}

func TestHandlerKeepsMissingAssetsOutOfSPA(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/assets/missing.js", nil)
	res := httptest.NewRecorder()
	Handler().ServeHTTP(res, req)
	if res.Code != http.StatusNotFound {
		t.Fatalf("missing asset status = %d body = %s", res.Code, res.Body.String())
	}
}

func TestPWAAssetHeaders(t *testing.T) {
	tests := []struct {
		path   string
		header string
		want   string
	}{
		{path: "/", header: "Cache-Control", want: "no-cache, no-store, must-revalidate"},
		{path: "/manifest.webmanifest", header: "Content-Type", want: "application/manifest+json"},
		{path: "/manifest.webmanifest", header: "Cache-Control", want: "no-cache, no-store, must-revalidate"},
		{path: "/sw.js", header: "Cache-Control", want: "no-cache, no-store, must-revalidate"},
		{path: "/sw.js", header: "Service-Worker-Allowed", want: "/"},
	}

	for _, test := range tests {
		t.Run(test.path, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, test.path, nil)
			response := httptest.NewRecorder()

			Handler().ServeHTTP(response, request)

			if response.Code != http.StatusOK {
				t.Fatalf("status = %d, want %d", response.Code, http.StatusOK)
			}
			if got := response.Header().Get(test.header); got != test.want {
				t.Fatalf("%s = %q, want %q", test.header, got, test.want)
			}
		})
	}
}
