package core

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"testing"

	"github.com/patriceckhart/zot/packages/provider"
	_ "modernc.org/sqlite"
)

func newSQLiteSessionRoot(t *testing.T, namespace string) string {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(t.TempDir(), "sessions.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := ConfigureSQLSessionStorage(context.Background(), db, "sqlite"); err != nil {
		db.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ResetSQLSessionStorage()
		_ = db.Close()
	})
	return SQLSessionsRoot(namespace)
}

func TestSQLDialectSyntax(t *testing.T) {
	mysqlSchema, mysqlIndex := sqlSessionSchema("mysql")
	if !strings.Contains(mysqlSchema, "LONGTEXT") || !strings.Contains(mysqlSchema, "VARCHAR(36)") || !strings.Contains(mysqlSchema, "utf8mb4") {
		t.Fatalf("MySQL schema uses incompatible column types or charset: %s", mysqlSchema)
	}
	if strings.Contains(mysqlIndex, "cwd") {
		t.Fatalf("MySQL index includes unbounded TEXT cwd: %s", mysqlIndex)
	}

	tests := []struct {
		dialect string
		bind    string
		append  string
	}{
		{dialect: "sqlite", bind: "?", append: "data || ?"},
		{dialect: "postgres", bind: "$1", append: "data || $1"},
		{dialect: "mysql", bind: "?", append: "CONCAT(data, ?)"},
	}
	for _, test := range tests {
		backend := &sqlSessionBackend{dialect: test.dialect}
		if got := backend.bind(1); got != test.bind {
			t.Errorf("%s bind = %q, want %q", test.dialect, got, test.bind)
		}
		if got := backend.appendExpression(); got != test.append {
			t.Errorf("%s append = %q, want %q", test.dialect, got, test.append)
		}
	}
}

func TestSQLSessionStorageRoundTripAndManagement(t *testing.T) {
	root := newSQLiteSessionRoot(t, "test")
	cwd := "/workspace/project"

	session, err := NewSession(root, cwd, "anthropic", "claude-test", "test")
	if err != nil {
		t.Fatal(err)
	}
	if want := "zot-sql://test/"; len(session.Path) <= len(want) || session.Path[:len(want)] != want {
		t.Fatalf("session path = %q, want prefix %q", session.Path, want)
	}
	message := provider.Message{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: "hello from SQL"}}}
	if err := session.AppendMessage(message); err != nil {
		t.Fatal(err)
	}
	if err := session.AppendUsage(provider.Usage{InputTokens: 3}, provider.Usage{InputTokens: 3, CostUSD: 0.25}); err != nil {
		t.Fatal(err)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}

	if got := LatestSession(root, cwd); got != session.Path {
		t.Fatalf("LatestSession = %q, want %q", got, session.Path)
	}
	reopened, messages, err := OpenSession(session.Path)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 1 || firstTextFromMessage(messages[0]) != "hello from SQL" {
		t.Fatalf("messages = %#v", messages)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}

	usage, err := SessionUsage(session.Path)
	if err != nil {
		t.Fatal(err)
	}
	if usage.InputTokens != 3 || usage.CostUSD != 0.25 {
		t.Fatalf("usage = %#v", usage)
	}
	if err := RenameSession(session.Path, "database session"); err != nil {
		t.Fatal(err)
	}
	summaries := DescribeSessions(root, cwd)
	if len(summaries) != 1 || summaries[0].Title != "database session" {
		t.Fatalf("summaries = %#v", summaries)
	}
	if err := DeleteSession(session.Path); err != nil {
		t.Fatal(err)
	}
	if got := ListSessions(root, cwd); len(got) != 0 {
		t.Fatalf("sessions after delete = %v", got)
	}
}

func TestSQLSessionStorageSupportsBranchAndPortableExport(t *testing.T) {
	root := newSQLiteSessionRoot(t, "portable")
	cwd := "/workspace"
	parent, err := NewSession(root, cwd, "test", "model", "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := parent.AppendMessage(provider.Message{Role: provider.RoleUser, Content: []provider.Content{provider.TextBlock{Text: "first prompt"}}}); err != nil {
		t.Fatal(err)
	}
	if err := parent.AppendMessage(provider.Message{Role: provider.RoleAssistant, Content: []provider.Content{provider.TextBlock{Text: "first answer"}}}); err != nil {
		t.Fatal(err)
	}

	branchPath, err := BranchSession(parent.Path, root, cwd, "test", 1)
	if err != nil {
		t.Fatal(err)
	}
	branch, messages, err := OpenSession(branchPath)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 1 || firstTextFromMessage(messages[0]) != "first prompt" {
		t.Fatalf("branch messages = %#v", messages)
	}
	_ = branch.Close()

	exported, err := ExportSession(parent.Path, filepath.Join(t.TempDir(), "export"+PortableExt))
	if err != nil {
		t.Fatal(err)
	}
	importedPath, err := ImportSession(exported, root, cwd, "test")
	if err != nil {
		t.Fatal(err)
	}
	imported, importedMessages, err := OpenSession(importedPath)
	if err != nil {
		t.Fatal(err)
	}
	defer imported.Close()
	if len(importedMessages) != 2 {
		t.Fatalf("imported messages = %#v", importedMessages)
	}
}

func TestSQLSessionStorageRemovesFreshEmptySession(t *testing.T) {
	root := newSQLiteSessionRoot(t, "empty")
	session, err := NewSession(root, "/workspace", "test", "model", "test")
	if err != nil {
		t.Fatal(err)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	if got := ListSessions(root, "/workspace"); len(got) != 0 {
		t.Fatalf("empty session was retained: %v", got)
	}
}
