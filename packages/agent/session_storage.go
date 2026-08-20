package agent

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	mysqlcfg "github.com/go-sql-driver/mysql"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/patriceckhart/zot/packages/core"
	_ "modernc.org/sqlite"
)

const sessionDatabaseURLEnv = "ZOT_SESSION_DATABASE_URL"

// SessionStorageConfig selects where main zot sessions are persisted.
// Backend may be empty/filesystem, sqlite, postgres, or mysql. Path overrides
// the filesystem root. DatabaseURL is a database/sql data source name.
// ZOT_SESSION_DATABASE_URL overrides it so a password does not need to be
// stored in config.json.
type SessionStorageConfig struct {
	Backend     string `json:"backend,omitempty"`
	Path        string `json:"path,omitempty"`
	DatabaseURL string `json:"database_url,omitempty"`
}

func (c SessionStorageConfig) normalizedBackend() string {
	backend := strings.ToLower(strings.TrimSpace(c.Backend))
	if backend == "" || backend == "file" || backend == "fs" {
		return "filesystem"
	}
	if backend == "postgresql" {
		return "postgres"
	}
	if backend == "mariadb" {
		return "mysql"
	}
	return backend
}

func (c SessionStorageConfig) databaseURL() string {
	if value := strings.TrimSpace(os.Getenv(sessionDatabaseURLEnv)); value != "" {
		return value
	}
	return strings.TrimSpace(c.DatabaseURL)
}

func validateSessionStorageConfig(c SessionStorageConfig) error {
	switch c.normalizedBackend() {
	case "filesystem":
		if c.Path != "" && !filepath.IsAbs(c.Path) {
			return errors.New("session_storage.path must be absolute")
		}
		return nil
	case "sqlite":
		return nil
	case "postgres", "mysql":
		if c.databaseURL() == "" {
			return fmt.Errorf("session_storage.database_url is required for %s (or set ZOT_SESSION_DATABASE_URL)", c.normalizedBackend())
		}
		return nil
	default:
		return fmt.Errorf("unsupported session_storage.backend %q (use filesystem, sqlite, postgres, or mysql)", c.Backend)
	}
}

func openSessionStorageDatabase(c SessionStorageConfig) (*sql.DB, error) {
	if err := validateSessionStorageConfig(c); err != nil {
		return nil, err
	}
	backend := c.normalizedBackend()
	if backend == "filesystem" {
		return nil, nil
	}
	dsn := c.databaseURL()
	driver := "pgx"
	if backend == "mysql" {
		driver = "mysql"
	}
	if backend == "sqlite" {
		driver = "sqlite"
		if dsn == "" {
			if err := os.MkdirAll(ZotHome(), 0o700); err != nil {
				return nil, fmt.Errorf("session storage: create zot home: %w", err)
			}
			dsn = filepath.Join(ZotHome(), "sessions.db")
		}
	}
	db, err := sql.Open(driver, dsn)
	if err != nil {
		return nil, fmt.Errorf("session storage: open %s: %w", backend, redactSessionStorageError(err, c))
	}
	return db, nil
}

func redactSessionStorageError(err error, c SessionStorageConfig) error {
	if err == nil {
		return nil
	}
	message := err.Error()
	dsn := c.databaseURL()
	if dsn != "" {
		message = strings.ReplaceAll(message, dsn, "<redacted>")
		if parsed, parseErr := url.Parse(dsn); parseErr == nil && parsed.User != nil {
			if password, ok := parsed.User.Password(); ok && password != "" {
				message = strings.ReplaceAll(message, password, "<redacted>")
			}
		}
		if parsed, parseErr := mysqlcfg.ParseDSN(dsn); parseErr == nil && parsed.Passwd != "" {
			message = strings.ReplaceAll(message, parsed.Passwd, "<redacted>")
		}
	}
	return errors.New(message)
}

func protectSQLiteSessionDatabase(c SessionStorageConfig) {
	if c.normalizedBackend() != "sqlite" {
		return
	}
	dsn := c.databaseURL()
	if dsn == "" {
		dsn = filepath.Join(ZotHome(), "sessions.db")
	}
	if dsn == ":memory:" {
		return
	}
	if strings.HasPrefix(dsn, "file:") {
		parsed, err := url.Parse(dsn)
		if err != nil || parsed.Path == "" {
			return
		}
		dsn = parsed.Path
	}
	_ = os.Chmod(dsn, 0o600)
}

func validateSessionStorageConnection(ctx context.Context, c SessionStorageConfig) error {
	db, err := openSessionStorageDatabase(c)
	if err != nil || db == nil {
		return err
	}
	defer db.Close()
	validateCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := db.PingContext(validateCtx); err != nil {
		return fmt.Errorf("session storage: connect: %w", redactSessionStorageError(err, c))
	}
	protectSQLiteSessionDatabase(c)
	return nil
}

// configureSessionStorage validates the configured credentials, opens the
// database/sql backend, checks connectivity, and initializes the schema.
// Filesystem storage remains the default and requires no setup.
func configureSessionStorage(ctx context.Context, c SessionStorageConfig) (func(), error) {
	if c.normalizedBackend() == "filesystem" && c.Path != "" {
		if err := os.MkdirAll(c.Path, 0o700); err != nil {
			return nil, fmt.Errorf("session storage: create filesystem root: %w", err)
		}
		info, err := os.Stat(c.Path)
		if err != nil {
			return nil, fmt.Errorf("session storage: inspect filesystem root: %w", err)
		}
		if !info.IsDir() {
			return nil, errors.New("session storage: filesystem path is not a directory")
		}
	}
	db, err := openSessionStorageDatabase(c)
	if err != nil {
		return nil, err
	}
	backend := c.normalizedBackend()
	if db == nil {
		core.ResetSQLSessionStorage()
		return func() {}, nil
	}
	validateCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := core.ConfigureSQLSessionStorage(validateCtx, db, backend); err != nil {
		_ = db.Close()
		return nil, redactSessionStorageError(err, c)
	}
	protectSQLiteSessionDatabase(c)
	return func() {
		core.ResetSQLSessionStorage()
		_ = db.Close()
	}, nil
}

func (configSettingsStore) SetSessionStorage(backend, path, databaseURL string) error {
	cfg, err := LoadConfig()
	if err != nil {
		return err
	}
	candidate := SessionStorageConfig{
		Backend:     backend,
		Path:        strings.TrimSpace(path),
		DatabaseURL: strings.TrimSpace(databaseURL),
	}
	if err := validateSessionStorageConfig(candidate); err != nil {
		return err
	}
	if candidate.normalizedBackend() == "filesystem" {
		if candidate.Path != "" {
			if err := os.MkdirAll(candidate.Path, 0o700); err != nil {
				return fmt.Errorf("session storage: create filesystem root: %w", err)
			}
			info, err := os.Stat(candidate.Path)
			if err != nil {
				return fmt.Errorf("session storage: inspect filesystem root: %w", err)
			}
			if !info.IsDir() {
				return errors.New("session storage: filesystem path is not a directory")
			}
		}
	} else if err := validateSessionStorageConnection(context.Background(), candidate); err != nil {
		return err
	}
	cfg.SessionStorage = candidate
	return SaveConfig(cfg)
}

func configuredSessionRoot(zotHome, agentName string) string {
	cfg, err := LoadConfig()
	if err == nil {
		if cfg.SessionStorage.normalizedBackend() != "filesystem" {
			namespace := "default"
			if agentName != "" {
				namespace = "agent:" + safeAgentName(agentName)
			}
			return core.SQLSessionsRoot(namespace)
		}
		if cfg.SessionStorage.Path != "" {
			zotHome = cfg.SessionStorage.Path
		}
	}
	if agentName == "" {
		return zotHome
	}
	return filepath.Join(zotHome, "sessions", "agents", safeAgentName(agentName))
}
