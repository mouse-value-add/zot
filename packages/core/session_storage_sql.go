package core

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const sqlSessionScheme = "zot-sql"

type sqlSessionBackend struct {
	db      *sql.DB
	dialect string
}

var configuredSQLSessions struct {
	sync.RWMutex
	backend *sqlSessionBackend
}

// ConfigureSQLSessionStorage installs the database/sql connection used by SQL
// session roots and creates zot's private session table. The caller owns db and
// must keep it open while sessions are in use. Supported dialects are sqlite,
// postgres, and mysql (including MariaDB).
func ConfigureSQLSessionStorage(ctx context.Context, db *sql.DB, dialect string) error {
	if db == nil {
		return errors.New("session storage: nil database")
	}
	dialect = strings.ToLower(strings.TrimSpace(dialect))
	if dialect != "sqlite" && dialect != "postgres" && dialect != "mysql" {
		return fmt.Errorf("session storage: unsupported SQL dialect %q", dialect)
	}
	if err := db.PingContext(ctx); err != nil {
		return fmt.Errorf("session storage: connect: %w", err)
	}
	schema, index := sqlSessionSchema(dialect)
	if _, err := db.ExecContext(ctx, schema); err != nil {
		return fmt.Errorf("session storage: create schema: %w", err)
	}
	if _, err := db.ExecContext(ctx, index); err != nil && !isDuplicateIndexError(err, dialect) {
		return fmt.Errorf("session storage: create index: %w", err)
	}
	configuredSQLSessions.Lock()
	configuredSQLSessions.backend = &sqlSessionBackend{db: db, dialect: dialect}
	configuredSQLSessions.Unlock()
	return nil
}

// ResetSQLSessionStorage removes the process-wide SQL session connection.
// It is primarily useful for tests. The caller remains responsible for closing db.
func ResetSQLSessionStorage() {
	configuredSQLSessions.Lock()
	configuredSQLSessions.backend = nil
	configuredSQLSessions.Unlock()
}

// SQLSessionsRoot returns an opaque root accepted by the session APIs. The
// namespace keeps normal runs and named Zotfile agents separate in one database.
func SQLSessionsRoot(namespace string) string {
	if strings.TrimSpace(namespace) == "" {
		namespace = "default"
	}
	return sqlSessionScheme + "://" + url.PathEscape(namespace)
}

func sqlBackend() (*sqlSessionBackend, error) {
	configuredSQLSessions.RLock()
	backend := configuredSQLSessions.backend
	configuredSQLSessions.RUnlock()
	if backend == nil {
		return nil, errors.New("session storage: SQL backend is not configured")
	}
	return backend, nil
}

func parseSQLRoot(root string) (string, bool) {
	u, err := url.Parse(root)
	if err != nil || u.Scheme != sqlSessionScheme || u.Host == "" || u.Path != "" {
		return "", false
	}
	namespace, err := url.PathUnescape(u.Host)
	return namespace, err == nil && namespace != ""
}

func sqlSessionPath(namespace, id string) string {
	return SQLSessionsRoot(namespace) + "/" + id
}

func parseSQLSessionPath(path string) (namespace, id string, ok bool) {
	u, err := url.Parse(path)
	if err != nil || u.Scheme != sqlSessionScheme || u.Host == "" {
		return "", "", false
	}
	namespace, err = url.PathUnescape(u.Host)
	if err != nil {
		return "", "", false
	}
	id = strings.TrimPrefix(u.Path, "/")
	return namespace, id, namespace != "" && id != "" && !strings.Contains(id, "/")
}

func sqlSessionSchema(dialect string) (schema, index string) {
	schema = `CREATE TABLE IF NOT EXISTS zot_sessions (
		id TEXT PRIMARY KEY,
		namespace TEXT NOT NULL,
		cwd TEXT NOT NULL,
		started_at BIGINT NOT NULL,
		updated_at BIGINT NOT NULL,
		data TEXT NOT NULL
	)`
	index = `CREATE INDEX IF NOT EXISTS zot_sessions_lookup ON zot_sessions (namespace, cwd, updated_at)`
	if dialect == "mysql" {
		schema = `CREATE TABLE IF NOT EXISTS zot_sessions (
			id VARCHAR(36) PRIMARY KEY,
			namespace VARCHAR(191) NOT NULL,
			cwd TEXT NOT NULL,
			started_at BIGINT NOT NULL,
			updated_at BIGINT NOT NULL,
			data LONGTEXT NOT NULL
		) DEFAULT CHARACTER SET utf8mb4 COLLATE utf8mb4_unicode_ci`
		// MySQL and MariaDB cannot portably index an unbounded TEXT cwd.
		// Filtering still uses cwd; this index narrows by namespace and recency.
		index = `CREATE INDEX zot_sessions_lookup ON zot_sessions (namespace, updated_at)`
	}
	return schema, index
}

func isDuplicateIndexError(err error, dialect string) bool {
	if err == nil || dialect != "mysql" {
		return false
	}
	message := strings.ToLower(err.Error())
	return strings.Contains(message, "duplicate key name") || strings.Contains(message, "already exists")
}

func (b *sqlSessionBackend) bind(n int) string {
	if b.dialect == "postgres" {
		return fmt.Sprintf("$%d", n)
	}
	return "?"
}

func (b *sqlSessionBackend) appendExpression() string {
	if b.dialect == "mysql" {
		return "CONCAT(data, " + b.bind(1) + ")"
	}
	return "data || " + b.bind(1)
}

func createStoredSession(root, cwd, id string) (string, io.WriteCloser, error) {
	if namespace, ok := parseSQLRoot(root); ok {
		backend, err := sqlBackend()
		if err != nil {
			return "", nil, err
		}
		now := time.Now().UTC().UnixNano()
		query := fmt.Sprintf("INSERT INTO zot_sessions (id, namespace, cwd, started_at, updated_at, data) VALUES (%s, %s, %s, %s, %s, %s)", backend.bind(1), backend.bind(2), backend.bind(3), backend.bind(4), backend.bind(5), backend.bind(6))
		if _, err := backend.db.Exec(query, id, namespace, cwd, now, now, ""); err != nil {
			return "", nil, err
		}
		path := sqlSessionPath(namespace, id)
		return path, &sqlSessionWriter{backend: backend, namespace: namespace, id: id}, nil
	}

	dir := SessionsDir(root, cwd)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", nil, err
	}
	name := fmt.Sprintf("%s-%s.jsonl", time.Now().UTC().Format("20060102-150405"), id[:8])
	path := filepath.Join(dir, name)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	return path, f, err
}

func openSessionReader(path string) (io.ReadCloser, error) {
	namespace, id, ok := parseSQLSessionPath(path)
	if !ok {
		return os.Open(path)
	}
	backend, err := sqlBackend()
	if err != nil {
		return nil, err
	}
	var data string
	query := fmt.Sprintf("SELECT data FROM zot_sessions WHERE namespace = %s AND id = %s", backend.bind(1), backend.bind(2))
	if err := backend.db.QueryRow(query, namespace, id).Scan(&data); err != nil {
		return nil, err
	}
	return io.NopCloser(strings.NewReader(data)), nil
}

func openSessionAppender(path string) (io.WriteCloser, error) {
	namespace, id, ok := parseSQLSessionPath(path)
	if !ok {
		return os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	}
	backend, err := sqlBackend()
	if err != nil {
		return nil, err
	}
	var exists int
	query := fmt.Sprintf("SELECT 1 FROM zot_sessions WHERE namespace = %s AND id = %s", backend.bind(1), backend.bind(2))
	if err := backend.db.QueryRow(query, namespace, id).Scan(&exists); err != nil {
		return nil, err
	}
	return &sqlSessionWriter{backend: backend, namespace: namespace, id: id}, nil
}

type sqlSessionWriter struct {
	backend   *sqlSessionBackend
	namespace string
	id        string
}

func (w *sqlSessionWriter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	query := fmt.Sprintf("UPDATE zot_sessions SET data = %s, updated_at = %s WHERE namespace = %s AND id = %s", w.backend.appendExpression(), w.backend.bind(2), w.backend.bind(3), w.backend.bind(4))
	result, err := w.backend.db.Exec(query, string(p), time.Now().UTC().UnixNano(), w.namespace, w.id)
	if err != nil {
		return 0, err
	}
	if n, err := result.RowsAffected(); err == nil && n != 1 {
		return 0, sql.ErrNoRows
	}
	return len(p), nil
}

func (w *sqlSessionWriter) Close() error { return nil }

func removeStoredSession(path string) error {
	namespace, id, ok := parseSQLSessionPath(path)
	if !ok {
		return os.Remove(path)
	}
	backend, err := sqlBackend()
	if err != nil {
		return err
	}
	query := fmt.Sprintf("DELETE FROM zot_sessions WHERE namespace = %s AND id = %s", backend.bind(1), backend.bind(2))
	result, err := backend.db.Exec(query, namespace, id)
	if err != nil {
		return err
	}
	if n, err := result.RowsAffected(); err == nil && n == 0 {
		return os.ErrNotExist
	}
	return nil
}

func listSQLSessions(root, cwd string) ([]string, bool) {
	namespace, ok := parseSQLRoot(root)
	if !ok {
		return nil, false
	}
	backend, err := sqlBackend()
	if err != nil {
		return nil, true
	}
	query := fmt.Sprintf("SELECT id FROM zot_sessions WHERE namespace = %s AND cwd = %s ORDER BY updated_at DESC, id DESC", backend.bind(1), backend.bind(2))
	rows, err := backend.db.Query(query, namespace, cwd)
	if err != nil {
		return nil, true
	}
	defer rows.Close()
	var paths []string
	for rows.Next() {
		var id string
		if rows.Scan(&id) == nil {
			paths = append(paths, sqlSessionPath(namespace, id))
		}
	}
	return paths, true
}
