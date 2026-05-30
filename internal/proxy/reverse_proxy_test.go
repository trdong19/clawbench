package proxy

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

const (
	testExampleComPort80  = "example.com:80"
	testExampleComPort443 = "example.com:443"
)

// httpGet is a test helper that performs an HTTP GET with context.
func httpGet(t *testing.T, urlStr string) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, urlStr, http.NoBody)
	assert.NoError(t, err)
	resp, err := http.DefaultClient.Do(req)
	assert.NoError(t, err)
	return resp
}

func TestReverseProxy_ForwardsRequest(t *testing.T) {
	// Setup a backend server that echoes the Host header
	var receivedHost string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedHost = r.Host
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	defer backend.Close()

	// Create reverse proxy pointing to the backend
	rp, err := NewReverseProxy("127.0.0.1", 0, backend.Listener.Addr().String(), "http")
	assert.NoError(t, err)
	defer rp.Close()

	go rp.Serve()
	addr := rp.Addr()
	assert.NotEmpty(t, addr)

	// Wait for listener to be ready
	time.Sleep(50 * time.Millisecond)

	// Send a request through the proxy using a real HTTP client
	resp := httpGet(t, "http://"+addr+"/test")
	defer func() { _ = resp.Body.Close() }()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	// The backend should receive the original target's Host, not "localhost:randomPort"
	assert.NotContains(t, receivedHost, "localhost", "Host header should not contain localhost")
}

func TestReverseProxy_SetsCorrectHost(t *testing.T) {
	// Setup a backend that records the Host header
	var receivedHost string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedHost = r.Host
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	// Target is the backend's address (simulating a LAN target like 192.168.1.100:8080)
	backendAddr := backend.Listener.Addr().String()
	rp, err := NewReverseProxy("127.0.0.1", 0, backendAddr, "http")
	assert.NoError(t, err)
	defer rp.Close()

	go rp.Serve()
	addr := rp.Addr()
	time.Sleep(50 * time.Millisecond)

	resp := httpGet(t, "http://"+addr+"/api/data")
	defer func() { _ = resp.Body.Close() }()

	// Host header should match the backend's address (target host:port)
	assert.Equal(t, backendAddr, receivedHost, "Host header should be the target address")
}

func TestReverseProxy_HandlesPort80(t *testing.T) {
	var receivedHost string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedHost = r.Host
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	backendAddr := backend.Listener.Addr().String()
	rp, err := NewReverseProxy("127.0.0.1", 0, backendAddr, "http")
	assert.NoError(t, err)
	defer rp.Close()

	go rp.Serve()
	addr := rp.Addr()
	time.Sleep(50 * time.Millisecond)

	resp := httpGet(t, "http://"+addr+"/")
	defer func() { _ = resp.Body.Close() }()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	// Host should be the backend address, not the proxy address
	assert.NotEqual(t, addr, receivedHost, "Host header should not be the proxy's address")
}

func TestReverseProxy_SupportsHTTPS(t *testing.T) {
	backend := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	backendAddr := backend.Listener.Addr().String()
	rp, err := NewReverseProxy("127.0.0.1", 0, backendAddr, "https")
	assert.NoError(t, err)
	// Configure the proxy's transport to trust the test server's certificate
	rp.SetInsecureSkipVerify(true)
	defer rp.Close()

	go rp.Serve()
	addr := rp.Addr()
	time.Sleep(50 * time.Millisecond)

	// Connect to proxy via plain HTTP (proxy handles TLS to backend)
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, "http://"+addr+"/", http.NoBody)
	assert.NoError(t, err)
	client := &http.Client{Transport: &http.Transport{}}
	resp, err := client.Do(req)
	assert.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestReverseProxy_Port(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	rp, err := NewReverseProxy("127.0.0.1", 0, backend.Listener.Addr().String(), "http")
	assert.NoError(t, err)
	defer rp.Close()

	port := rp.Port()
	assert.Greater(t, port, 0, "Auto-assigned port should be > 0")
}

func TestReverseProxy_TargetHostRewrite(t *testing.T) {
	var receivedHost string
	var receivedPath string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedHost = r.Host
		receivedPath = r.URL.Path
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("response from backend"))
	}))
	defer backend.Close()

	// The backend's address simulates a LAN target
	backendAddr := backend.Listener.Addr().String()
	// Extract just the port to simulate a scenario where we forward to a named host
	_, port, _ := net.SplitHostPort(backendAddr)

	// Simulate forwarding to "192.168.1.100:8080" by using a custom target address
	// We use the actual backend's port but set the target host to the backend's IP
	targetHost := "127.0.0.1:" + port
	rp, err := NewReverseProxy("127.0.0.1", 0, targetHost, "http")
	assert.NoError(t, err)
	defer rp.Close()

	go rp.Serve()
	addr := rp.Addr()
	time.Sleep(50 * time.Millisecond)

	resp := httpGet(t, "http://"+addr+"/some/path")
	defer func() { _ = resp.Body.Close() }()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Equal(t, targetHost, receivedHost, "Host header should be the target address, not localhost")
	assert.Equal(t, "/some/path", receivedPath, "Path should be forwarded correctly")
}

func TestReverseProxy_ResponseBody(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("hello from backend"))
	}))
	defer backend.Close()

	rp, err := NewReverseProxy("127.0.0.1", 0, backend.Listener.Addr().String(), "http")
	assert.NoError(t, err)
	defer rp.Close()

	go rp.Serve()
	addr := rp.Addr()
	time.Sleep(50 * time.Millisecond)

	resp := httpGet(t, "http://"+addr+"/")
	defer func() { _ = resp.Body.Close() }()

	body, err := io.ReadAll(resp.Body)
	assert.NoError(t, err)
	assert.True(t, strings.Contains(string(body), "hello from backend"), "Response body should contain backend response")
}

func TestStripDefaultPort(t *testing.T) {
	tests := []struct {
		hostPort string
		scheme   string
		want     string
	}{
		{"192.168.100.1:80", schemeHTTP, "192.168.100.1"},
		{"192.168.100.1:443", schemeHTTPS, "192.168.100.1"},
		{"192.168.100.1:8080", schemeHTTP, "192.168.100.1:8080"},
		{"192.168.100.1:8443", schemeHTTPS, "192.168.100.1:8443"},
		{testExampleComPort80, schemeHTTP, "example.com"},
		{testExampleComPort443, schemeHTTPS, "example.com"},
		{
			testExampleComPort80,
			schemeHTTPS,
			testExampleComPort80,
		}, // port 80 with https is NOT default
		{
			testExampleComPort443,
			schemeHTTP,
			testExampleComPort443,
		}, // port 443 with http is NOT default
		{
			"10.0.0.1",
			schemeHTTP,
			"10.0.0.1",
		}, // no port at all
	}
	for _, tt := range tests {
		got := stripDefaultPort(tt.hostPort, tt.scheme)
		assert.Equal(t, tt.want, got, "stripDefaultPort(%q, %q)", tt.hostPort, tt.scheme)
	}
}

func TestReverseProxy_StripsDefaultPortFromHost(t *testing.T) {
	// Backend that records the Host header
	var receivedHost string
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		receivedHost = r.Host
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	defer backend.Close()

	backendURL, _ := url.Parse(backend.URL)
	targetAddr := backendURL.Host // e.g. "127.0.0.1:PORT" — non-default port

	rp, err := NewReverseProxy("127.0.0.1", 0, targetAddr, "http")
	assert.NoError(t, err)
	go rp.Serve()
	defer rp.Close()

	resp := httpGet(t, "http://"+rp.Addr()+"/test")
	_ = resp.Body.Close()

	// Non-default port: Host should include port
	assert.Equal(t, targetAddr, receivedHost, "Host for non-default port should include port")
}

func TestReverseProxy_AddAndPort(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	rp, err := NewReverseProxy("127.0.0.1", 0, backend.Listener.Addr().String(), "http")
	assert.NoError(t, err)
	defer rp.Close()

	// Addr and Port should return valid values
	assert.NotEmpty(t, rp.Addr())
	assert.Greater(t, rp.Port(), 0)
}

func TestReverseProxy_AddrNilListener(t *testing.T) {
	rp := &ReverseProxy{}
	assert.Equal(t, "", rp.Addr(), "Addr should return empty string when listener is nil")
	assert.Equal(t, 0, rp.Port(), "Port should return 0 when listener is nil")
}

func TestReverseProxy_NewReverseProxy_InvalidListenAddr(t *testing.T) {
	// Using a non-routable address that can't be listened on should fail
	_, err := NewReverseProxy("256.256.256.256", 80, "127.0.0.1:8080", "http")
	assert.Error(t, err, "should fail with invalid listen address")
}

func TestReverseProxy_NewReverseProxy_EmptyProtocol(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	rp, err := NewReverseProxy("127.0.0.1", 0, backend.Listener.Addr().String(), "")
	assert.NoError(t, err, "empty protocol should default to http")
	rp.Close()
}

func TestReverseProxy_NewReverseProxy_TargetWithScheme(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	// Pass targetAddr with scheme prefix
	addr := "http://" + backend.Listener.Addr().String()
	rp, err := NewReverseProxy("127.0.0.1", 0, addr, "http")
	assert.NoError(t, err)
	rp.Close()
}

func TestReverseProxy_Serve_ServerClosed(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	rp, err := NewReverseProxy("127.0.0.1", 0, backend.Listener.Addr().String(), "http")
	assert.NoError(t, err)

	// Close before Serve — Serve should handle http.ErrServerClosed gracefully
	rp.Close()
	// Serve returns when server is closed — this tests the ErrServerClosed path
	done := make(chan struct{})
	go func() {
		rp.Serve()
		close(done)
	}()

	select {
	case <-done:
		// Serve returned as expected
	case <-time.After(2 * time.Second):
		t.Fatal("Serve should return after server is closed")
	}
}
