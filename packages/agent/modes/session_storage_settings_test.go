package modes

import (
	"errors"
	"testing"
)

func TestSessionStorageSettingsShowBackendSpecificField(t *testing.T) {
	tests := []struct {
		backend string
		wantKey string
		hideKey string
	}{
		{backend: "filesystem", wantKey: "session_storage_path", hideKey: "session_database_url"},
		{backend: "sqlite", wantKey: "session_database_url", hideKey: "session_storage_path"},
		{backend: "postgres", wantKey: "session_database_url", hideKey: "session_storage_path"},
		{backend: "mysql", wantKey: "session_database_url", hideKey: "session_storage_path"},
	}
	for _, test := range tests {
		t.Run(test.backend, func(t *testing.T) {
			interactive := NewInteractive(InteractiveConfig{SessionStorageBackend: test.backend})
			items := interactive.sessionStorageSettingItems()
			if !hasSettingsKey(items, test.wantKey) {
				t.Fatalf("items for %s are missing %s", test.backend, test.wantKey)
			}
			if hasSettingsKey(items, test.hideKey) {
				t.Fatalf("items for %s unexpectedly include %s", test.backend, test.hideKey)
			}
		})
	}
}

func TestSessionStorageSettingsIncludeMySQLAndMariaDB(t *testing.T) {
	interactive := NewInteractive(InteractiveConfig{})
	items := interactive.sessionStorageSettingItems()
	if len(items) == 0 {
		t.Fatal("missing backend setting")
	}
	for _, option := range items[0].options {
		if option.value == "mysql" && option.label == "MySQL / MariaDB" {
			return
		}
	}
	t.Fatal("missing MySQL / MariaDB backend option")
}

func TestFailedDatabaseSelectionShowsCredentialFieldWithoutSaving(t *testing.T) {
	store := &sessionStorageSettingsStub{err: errors.New("database URL is required")}
	interactive := NewInteractive(InteractiveConfig{
		SessionStorageBackend: "filesystem",
		SettingsStore:         store,
	})
	interactive.settingsDialog.Open([]settingsItem{{
		key:      "session_storage",
		label:    "session storage",
		children: interactive.sessionStorageSettingItems(),
	}})
	interactive.settingsDialog.toggleCurrent()
	interactive.applySessionStorageBackendSetting("postgres")

	if interactive.cfg.SessionStorageBackend != "postgres" {
		t.Fatalf("pending backend = %q, want postgres", interactive.cfg.SessionStorageBackend)
	}
	if !hasSettingsKey(interactive.settingsDialog.items, "session_database_url") {
		t.Fatal("database URL field was not shown after selecting PostgreSQL")
	}
	if store.saved {
		t.Fatal("invalid database configuration was saved")
	}
}

type sessionStorageSettingsStub struct {
	SettingsStore
	err   error
	saved bool
}

func (s *sessionStorageSettingsStub) SetSessionStorage(backend, path, databaseURL string) error {
	if s.err != nil {
		return s.err
	}
	s.saved = true
	return nil
}

func hasSettingsKey(items []settingsItem, key string) bool {
	for _, item := range items {
		if item.key == key {
			return true
		}
	}
	return false
}
