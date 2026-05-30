package model

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const (
	testEnvKey        = "KEY"
	testEnvHelloWorld = "hello world"
)

func TestParseEnvLine(t *testing.T) {
	tests := []struct {
		name      string
		line      string
		wantKey   string
		wantValue string
		wantErr   bool
	}{
		{"simple", testEnvKey + "=value", testEnvKey, "value", false},
		{"quoted double", testEnvKey + `="hello world"`, testEnvKey, testEnvHelloWorld, false},
		{"quoted single", testEnvKey + `='hello world'`, testEnvKey, testEnvHelloWorld, false},
		{"empty value", testEnvKey + "=", testEnvKey, "", false},
		{"value with spaces", testEnvKey + "=hello world", testEnvKey, testEnvHelloWorld, false},
		{"inline comment", testEnvKey + "=value # comment", testEnvKey, "value", false},
		{"no equals", "KEYVALUE", "", "", true},
		{"equals at start", "=value", "", "", true},
		{"underscores", "MY_KEY=my_value", "MY_KEY", "my_value", false},
		{"quoted with equals", testEnvKey + `="a=b"`, testEnvKey, "a=b", false},
		{"quoted empty", testEnvKey + `=""`, testEnvKey, "", false},
		{"number value", "PORT=3000", "PORT", "3000", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			key, value, err := parseEnvLine(tt.line)
			if tt.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
				assert.Equal(t, tt.wantKey, key)
				assert.Equal(t, tt.wantValue, value)
			}
		})
	}
}

func TestLoadDotEnv(t *testing.T) {
	// Create a temp .env file
	dir := t.TempDir()
	envPath := filepath.Join(dir, ".env")

	content := `# This is a comment
API_KEY=sk-test-123
PORT=3000

# Another comment
DB_URL="postgres://localhost:5432/mydb"
EMPTY_VAR=
`
	err := os.WriteFile(envPath, []byte(content), 0o600)
	assert.NoError(t, err)

	// Clear any existing values
	require.NoError(t, os.Unsetenv("API_KEY"))
	require.NoError(t, os.Unsetenv("PORT"))
	require.NoError(t, os.Unsetenv("DB_URL"))
	require.NoError(t, os.Unsetenv("EMPTY_VAR"))

	err = LoadDotEnv(envPath)
	assert.NoError(t, err)

	assert.Equal(t, "sk-test-123", os.Getenv("API_KEY"))
	assert.Equal(t, "3000", os.Getenv("PORT"))
	assert.Equal(t, "postgres://localhost:5432/mydb", os.Getenv("DB_URL"))
	assert.Equal(t, "", os.Getenv("EMPTY_VAR"))
}

func TestLoadDotEnvNotFound(t *testing.T) {
	err := LoadDotEnv("/nonexistent/.env")
	assert.Error(t, err)
}

func TestLoadDotEnvInvalidLine(t *testing.T) {
	dir := t.TempDir()
	envPath := filepath.Join(dir, ".env")

	err := os.WriteFile(envPath, []byte("INVALID_LINE_NO_EQUALS\n"), 0o600)
	assert.NoError(t, err)

	err = LoadDotEnv(envPath)
	assert.Error(t, err)
}

func TestLoadDotEnv_InheritableBySubprocess(t *testing.T) {
	// Verify that variables loaded by LoadDotEnv are visible via os.Environ(),
	// which is what CLI subprocesses inherit when cmd.Env = os.Environ().
	dir := t.TempDir()
	envPath := filepath.Join(dir, ".env")

	// Use a unique key to avoid collisions with other tests
	key := "CLAWBENCH_TEST_DOTENV_KEY"
	value := "test-value-from-dotenv"

	require.NoError(t, os.Unsetenv(key))
	t.Cleanup(func() { require.NoError(t, os.Unsetenv(key)) })

	err := os.WriteFile(envPath, []byte(key+"="+value+"\n"), 0o600)
	assert.NoError(t, err)

	err = LoadDotEnv(envPath)
	assert.NoError(t, err)

	// Must be in os.Environ() (the slice used by cmd.Env = os.Environ())
	found := false
	for _, entry := range os.Environ() {
		if entry == key+"="+value {
			found = true
			break
		}
	}
	assert.True(t, found, "expected %s to appear in os.Environ() after LoadDotEnv", key)

	// Also verify via os.Getenv
	assert.Equal(t, value, os.Getenv(key))
}

func TestLoadDotEnv_OverwritesExisting(t *testing.T) {
	// LoadDotEnv should overwrite existing env vars (matching dotenv convention)
	dir := t.TempDir()
	envPath := filepath.Join(dir, ".env")

	key := "CLAWBENCH_TEST_OVERWRITE_KEY"
	require.NoError(t, os.Setenv(key, "old-value"))
	t.Cleanup(func() { require.NoError(t, os.Unsetenv(key)) })

	err := os.WriteFile(envPath, []byte(key+"=new-value\n"), 0o600)
	assert.NoError(t, err)

	err = LoadDotEnv(envPath)
	assert.NoError(t, err)

	assert.Equal(t, "new-value", os.Getenv(key))
}
