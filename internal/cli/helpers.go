// Package cli implements the ClawBench command-line interface for task management, RAG search, and coverage reporting.
package cli

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"

	"clawbench/internal/model"

	"gopkg.in/yaml.v3"
)

const (
	cookieProjectName = "clawbench_project"
	jsonKeyError      = "error"
)

// FindConfigPath searches for config.yaml in priority order:
//  1. <BinDir>/config/config.yaml (green portable: next to binary)
//  2. config/config.yaml (CWD-relative, standard layout)
func FindConfigPath(binDir string) string {
	configPath := filepath.Join(binDir, "config", "config.yaml")
	if _, err := os.Stat(configPath); os.IsNotExist(err) {
		configPath = filepath.Join("config", "config.yaml")
	}
	return configPath
}

// loadConfig loads the YAML config file and applies defaults.
// It is safe to call multiple times — subsequent calls are no-ops
// once model.ConfigInstance is populated.
func loadConfig() {
	if model.ConfigInstance.Port != 0 {
		return // already loaded
	}

	absBinPath, err := filepath.Abs(os.Args[0])
	if err != nil {
		fmt.Fprintf(os.Stderr, "Warning: failed to resolve binary path: %v\n", err)
		absBinPath = os.Args[0]
	}
	model.BinDir = filepath.Dir(absBinPath)

	var cfg model.Config
	var presence map[string]bool
	configPath := FindConfigPath(model.BinDir)

	data, err := os.ReadFile(configPath)
	if err == nil {
		var raw map[string]any
		if err := yaml.Unmarshal(data, &raw); err != nil {
			fmt.Fprintf(os.Stderr, "Failed to parse config: %v\n", err)
			return
		}
		presence = model.ParsePresenceMap(raw)
		if err := yaml.Unmarshal(data, &cfg); err != nil {
			fmt.Fprintf(os.Stderr, "Failed to parse config: %v\n", err)
			return
		}
	}
	model.ApplyDefaults(&cfg, presence)
	model.ConfigInstance = cfg
}

// apiURL returns the base URL for the local server API.
// Uses https:// when TLS is enabled in config, otherwise http://.
func apiURL() string {
	port := model.ConfigInstance.Port
	if port == 0 {
		port = 20000
	}
	scheme := "http"
	if model.ConfigInstance.TLS.Enabled {
		scheme = "https"
	}
	return scheme + "://localhost:" + strconv.Itoa(port)
}

// httpClient returns an HTTP client that skips TLS verification.
// CLI connects to localhost — self-signed certs are expected.
var httpClient = &http.Client{
	Transport: &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true}, //nolint:gosec // local/SSH tunnel connection, certificate verification not applicable
	},
}

// httpDo performs an HTTP request to the server API.
// No auth needed — CLI runs on localhost which is auto-trusted by the server.
func httpDo(method, path string, body any) (result map[string]any, statusCode int, err error) {
	var reqBody io.Reader
	if body != nil {
		b, marshalErr := json.Marshal(body)
		if marshalErr != nil {
			return nil, 0, fmt.Errorf("marshal request: %w", marshalErr)
		}
		reqBody = bytes.NewReader(b)
	}

	req, err := http.NewRequestWithContext(context.Background(), method, apiURL()+path, reqBody)
	if err != nil {
		return nil, 0, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("server not reachable at %s: %w", apiURL(), err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("read response: %w", err)
	}

	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, resp.StatusCode, fmt.Errorf("parse response: %w (body: %s)", err, string(respBody))
	}

	return result, resp.StatusCode, nil
}

// httpDoWithProject is like httpDo but sets the clawbench_project cookie
// so the server can bind the operation to the correct project.
func httpDoWithProject(method, path string, body any, projectPath string) (result map[string]any, statusCode int, err error) {
	var reqBody io.Reader
	if body != nil {
		b, marshalErr := json.Marshal(body)
		if marshalErr != nil {
			return nil, 0, fmt.Errorf("marshal request: %w", marshalErr)
		}
		reqBody = bytes.NewReader(b)
	}

	req, err := http.NewRequestWithContext(context.Background(), method, apiURL()+path, reqBody)
	if err != nil {
		return nil, 0, fmt.Errorf("create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	// Set project cookie so server's requireProject() can extract it
	if projectPath != "" {
		req.AddCookie(&http.Cookie{ //nolint:gosec // local network only, no HTTPS; Secure flag would prevent functionality
			Name:     cookieProjectName,
			Value:    url.QueryEscape(projectPath),
			HttpOnly: true,
			SameSite: http.SameSiteStrictMode,
			Secure:   model.ConfigInstance.TLS.Enabled,
		})
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, 0, fmt.Errorf("server not reachable at %s: %w", apiURL(), err)
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, resp.StatusCode, fmt.Errorf("read response: %w", err)
	}

	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, resp.StatusCode, fmt.Errorf("parse response: %w (body: %s)", err, string(respBody))
	}

	return result, resp.StatusCode, nil
}

// outputJSON prints v as JSON to stdout.
func outputJSON(v any) {
	b, err := json.Marshal(v)
	if err != nil {
		slog.Error("failed to marshal JSON", slog.String("err", err.Error())) //nolint:sloglint // error handling path
		return
	}
	fmt.Println(string(b))
}

// outputError prints a JSON error and returns exit code 1.
func outputError(msg string) int {
	outputJSON(map[string]any{"ok": false, jsonKeyError: msg})
	return 1
}

// mustMarshal returns the JSON encoding of v, or "{}" on error.
func mustMarshal(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return "{}"
	}
	return string(b)
}

// flagSet creates a FlagSet with output directed to io.Discard
// (custom help is handled by parseOrHelp).
func flagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	return fs
}
