package middleware_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"

	"clawbench/internal/middleware"
	"clawbench/internal/model"

	"github.com/stretchr/testify/assert"
)

const (
	testValidToken    = "valid-token"
	testRemoteAddr    = "192.168.1.100:12345"
	testProjectCookie = "clawbench_project"
)

// okHandler is an always-200 handler used as the "next" in middleware chains.
func okHandler(_ http.ResponseWriter, _ *http.Request) {}

// withSavedToken saves model.SessionToken and model.CookieToken, runs f, then restores them.
func withSavedToken(f func()) {
	origSession := model.SessionToken
	origCookie := model.CookieToken
	defer func() {
		model.SessionToken = origSession
		model.CookieToken = origCookie
	}()
	f()
}

// --- Auth: no password configured ---

func TestAuth_NoPassword_PassThrough(t *testing.T) {
	withSavedToken(func() {
		model.SessionToken = ""

		rec := httptest.NewRecorder()
		req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil)

		middleware.Auth(okHandler).ServeHTTP(rec, req)

		assert.Equal(t, http.StatusOK, rec.Code)
	})
}

// --- Auth: localhost bypass ---

func TestAuth_Localhost_IPv4_BypassesAuth(t *testing.T) {
	withSavedToken(func() {
		model.SessionToken = testValidToken

		rec := httptest.NewRecorder()
		req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil)
		req.RemoteAddr = "127.0.0.1:12345"

		middleware.Auth(okHandler).ServeHTTP(rec, req)

		assert.Equal(t, http.StatusOK, rec.Code)
	})
}

func TestAuth_Localhost_IPv6_BypassesAuth(t *testing.T) {
	withSavedToken(func() {
		model.SessionToken = testValidToken

		rec := httptest.NewRecorder()
		req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil)
		req.RemoteAddr = "[::1]:12345"

		middleware.Auth(okHandler).ServeHTTP(rec, req)

		assert.Equal(t, http.StatusOK, rec.Code)
	})
}

// --- Auth: remote with valid cookie ---

func TestAuth_ValidCookie_PassThrough(t *testing.T) {
	withSavedToken(func() {
		model.SessionToken = testValidToken

		rec := httptest.NewRecorder()
		req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil)
		req.RemoteAddr = testRemoteAddr
		req.AddCookie(&http.Cookie{
			Name:  model.SessionCookie,
			Value: testValidToken,
		})

		middleware.Auth(okHandler).ServeHTTP(rec, req)

		assert.Equal(t, http.StatusOK, rec.Code)
	})
}

// --- Auth: remote with invalid/missing cookie ---

func TestAuth_InvalidCookieValue_Returns401(t *testing.T) {
	withSavedToken(func() {
		model.SessionToken = testValidToken

		rec := httptest.NewRecorder()
		req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil)
		req.RemoteAddr = testRemoteAddr
		req.AddCookie(&http.Cookie{
			Name:  model.SessionCookie,
			Value: "wrong-token",
		})

		middleware.Auth(okHandler).ServeHTTP(rec, req)

		assert.Equal(t, http.StatusUnauthorized, rec.Code)
	})
}

func TestAuth_MissingCookie_Returns401(t *testing.T) {
	withSavedToken(func() {
		model.SessionToken = testValidToken

		rec := httptest.NewRecorder()
		req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil)
		req.RemoteAddr = testRemoteAddr

		middleware.Auth(okHandler).ServeHTTP(rec, req)

		assert.Equal(t, http.StatusUnauthorized, rec.Code)
	})
}

// --- Auth: localhost + bad cookie still passes (localhost wins) ---

func TestAuth_LocalhostWithBadCookie_StillPasses(t *testing.T) {
	withSavedToken(func() {
		model.SessionToken = testValidToken

		rec := httptest.NewRecorder()
		req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil)
		req.RemoteAddr = "127.0.0.1:12345"
		req.AddCookie(&http.Cookie{
			Name:  model.SessionCookie,
			Value: "wrong-token",
		})

		middleware.Auth(okHandler).ServeHTTP(rec, req)

		assert.Equal(t, http.StatusOK, rec.Code)
	})
}

// --- GetProjectFromCookie ---

func TestGetProjectFromCookie_NormalExtraction(t *testing.T) {
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{
		Name:  testProjectCookie,
		Value: "/home/user/myproject",
	})

	result := middleware.GetProjectFromCookie(req)
	assert.Equal(t, "/home/user/myproject", result)
}

func TestGetProjectFromCookie_URLEncodedValueDecoded(t *testing.T) {
	encoded := url.QueryEscape("/home/user/my project")
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{
		Name:  testProjectCookie,
		Value: encoded,
	})

	result := middleware.GetProjectFromCookie(req)
	assert.Equal(t, "/home/user/my project", result)
}

func TestGetProjectFromCookie_NoCookie_ReturnsEmpty(t *testing.T) {
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil)

	result := middleware.GetProjectFromCookie(req)
	assert.Equal(t, "", result)
}

func TestGetProjectFromCookie_EmptyValue_ReturnsEmpty(t *testing.T) {
	req := httptest.NewRequestWithContext(context.Background(), http.MethodGet, "/", nil)
	req.AddCookie(&http.Cookie{
		Name:  testProjectCookie,
		Value: "",
	})

	result := middleware.GetProjectFromCookie(req)
	assert.Equal(t, "", result)
}
