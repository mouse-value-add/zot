package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/patriceckhart/zot/packages/core"
)

func TestValidateSessionStorageConfig(t *testing.T) {
	t.Setenv(sessionDatabaseURLEnv, "")
	tests := []struct {
		name    string
		config  SessionStorageConfig
		wantErr string
	}{
		{name: "default filesystem"},
		{name: "filesystem", config: SessionStorageConfig{Backend: "filesystem"}},
		{name: "filesystem custom root", config: SessionStorageConfig{Backend: "filesystem", Path: filepath.Join(t.TempDir(), "sessions")}},
		{name: "filesystem relative root", config: SessionStorageConfig{Backend: "filesystem", Path: "relative"}, wantErr: "must be absolute"},
		{name: "sqlite default path", config: SessionStorageConfig{Backend: "sqlite"}},
		{name: "postgres", config: SessionStorageConfig{Backend: "postgres", DatabaseURL: "postgres://user:pass@example.invalid/db"}},
		{name: "mysql", config: SessionStorageConfig{Backend: "mysql", DatabaseURL: "user:pass@tcp(localhost:3306)/zot"}},
		{name: "mariadb alias", config: SessionStorageConfig{Backend: "mariadb", DatabaseURL: "user:pass@tcp(localhost:3306)/zot"}},
		{name: "postgres missing URL", config: SessionStorageConfig{Backend: "postgres"}, wantErr: "database_url is required"},
		{name: "mysql missing URL", config: SessionStorageConfig{Backend: "mysql"}, wantErr: "database_url is required"},
		{name: "unknown", config: SessionStorageConfig{Backend: "oracle"}, wantErr: "unsupported"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := validateSessionStorageConfig(test.config)
			if test.wantErr == "" && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if test.wantErr != "" && (err == nil || !strings.Contains(err.Error(), test.wantErr)) {
				t.Fatalf("error = %v, want substring %q", err, test.wantErr)
			}
		})
	}
}

func TestSessionStorageErrorsRedactDatabaseCredentials(t *testing.T) {
	config := SessionStorageConfig{Backend: "postgres", DatabaseURL: "postgres://alice:top-secret@db.example/zot"}
	err := redactSessionStorageError(os.ErrInvalid, config)
	if strings.Contains(err.Error(), "top-secret") {
		t.Fatalf("error exposed password: %v", err)
	}
	withURL := redactSessionStorageError(&storageTestError{text: "failed postgres://alice:top-secret@db.example/zot"}, config)
	if strings.Contains(withURL.Error(), "top-secret") || strings.Contains(withURL.Error(), config.DatabaseURL) {
		t.Fatalf("error exposed database URL: %v", withURL)
	}
	mysqlConfig := SessionStorageConfig{Backend: "mysql", DatabaseURL: "alice:top-secret@tcp(db.example:3306)/zot"}
	mysqlErr := redactSessionStorageError(&storageTestError{text: "access denied for top-secret"}, mysqlConfig)
	if strings.Contains(mysqlErr.Error(), "top-secret") {
		t.Fatalf("error exposed MySQL password: %v", mysqlErr)
	}
}

type storageTestError struct{ text string }

func (e *storageTestError) Error() string { return e.text }

func TestConfigureSQLiteSessionStorageUsesDefaultDatabase(t *testing.T) {
	t.Setenv("ZOT_HOME", t.TempDir())
	cleanup, err := configureSessionStorage(context.Background(), SessionStorageConfig{Backend: "sqlite"})
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()

	root := configuredSessionRoot(ZotHome(), "")
	session, err := core.NewSession(root, "/workspace", "test", "model", "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(filepath.Join(ZotHome(), "sessions.db"))
	if err != nil {
		t.Fatalf("default SQLite database: %v", err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("SQLite database permissions = %o, want 600", got)
	}
}

func TestConfiguredSessionRootUsesFilesystemOverride(t *testing.T) {
	customRoot := filepath.Join(t.TempDir(), "session-root")
	t.Setenv("ZOT_HOME", t.TempDir())
	if err := SaveConfig(Config{SessionStorage: SessionStorageConfig{Backend: "filesystem", Path: customRoot}}); err != nil {
		t.Fatal(err)
	}
	if got := configuredSessionRoot(ZotHome(), ""); got != customRoot {
		t.Fatalf("root = %q, want %q", got, customRoot)
	}
	wantAgent := filepath.Join(customRoot, "sessions", "agents", "reviewer")
	if got := configuredSessionRoot(ZotHome(), "reviewer"); got != wantAgent {
		t.Fatalf("agent root = %q, want %q", got, wantAgent)
	}
}

func TestSettingsStoreValidatesAndPersistsSessionStorage(t *testing.T) {
	t.Setenv("ZOT_HOME", t.TempDir())
	store := configSettingsStore{}
	customRoot := filepath.Join(t.TempDir(), "filesystem")
	if err := store.SetSessionStorage("filesystem", customRoot, ""); err != nil {
		t.Fatal(err)
	}
	customPath := filepath.Join(t.TempDir(), "custom.db")
	if err := store.SetSessionStorage("sqlite", customRoot, customPath); err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.SessionStorage.Backend != "sqlite" || cfg.SessionStorage.Path != customRoot || cfg.SessionStorage.DatabaseURL != customPath {
		t.Fatalf("session storage = %#v", cfg.SessionStorage)
	}
	if err := store.SetSessionStorage("oracle", customRoot, customPath); err == nil {
		t.Fatal("unsupported backend was accepted")
	}
}

func TestSaveConfigProtectsDatabaseCredentials(t *testing.T) {
	t.Setenv("ZOT_HOME", t.TempDir())
	cfg := Config{SessionStorage: SessionStorageConfig{Backend: "postgres", DatabaseURL: "postgres://user:secret@localhost/zot"}}
	if err := SaveConfig(cfg); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(ConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("config permissions = %o, want 600", got)
	}
}
