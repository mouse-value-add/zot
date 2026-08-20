package modes

import (
	"strings"
	"testing"

	"github.com/patriceckhart/zot/packages/tui"
)

func TestSettingsDialogEditsAndMasksSecretText(t *testing.T) {
	dialog := newSettingsDialog()
	dialog.Open([]settingsItem{{
		key:         "session_database_url",
		label:       "database URL",
		text:        true,
		secret:      true,
		stringValue: "secret",
	}})

	lines := dialog.Render(tui.Dark, 80)
	rendered := strings.Join(lines, "\n")
	if strings.Contains(rendered, "secret") || !strings.Contains(rendered, "******") {
		t.Fatalf("secret was not masked: %q", rendered)
	}

	dialog.HandleKey(tui.Key{Kind: tui.KeyEnter})
	if !dialog.editing {
		t.Fatal("enter did not open text editor")
	}
	dialog.HandleKey(tui.Key{Kind: tui.KeyBackspace})
	action := dialog.HandleKey(tui.Key{Kind: tui.KeyEnter})
	if action.Key != "session_database_url" || action.StringValue != "secre" {
		t.Fatalf("action = %#v", action)
	}
}
