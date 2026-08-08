package config_test

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pigfox/render-env-sync/internal/config"
)

// configCanary stands in for whatever a user's config file happens to contain
// on a line the reader rejects. The file is supposed to hold ids and paths
// rather than secrets, but "supposed to" is not a guarantee, and an error
// message is the wrong place to find out.
const configCanary = "CANARY_c0nf1g_c0ntent_should_never_be_echoed"

// TestConfigErrorsNeverQuoteFileContent is named for the failure it prevents:
// the YAML reader rejecting a line and quoting it back, the same shape of bug
// that put a live API key on a terminal from the .env parser.
func TestConfigErrorsNeverQuoteFileContent(t *testing.T) {
	malformed := []struct {
		name string
		src  string
	}{
		{"not a mapping line", "version: 1\n" + configCanary + "\n"},
		{"bad version scalar", "version: " + configCanary + "\n"},
		{"bad page limit", "version: 1\ndefaults:\n  page_limit: " + configCanary + "\n"},
		{"bad prod flag", "version: 1\nprojects:\n  p:\n    environments:\n      e:\n        service: srv-x\n        prod: " + configCanary + "\n        env_files: /a\n"},
		{"bad home", "version: 1\nprojects:\n  p:\n    environments:\n      e:\n        service: srv-x\n        env_files: /a\n        manage:\n          K: " + configCanary + "\n"},
		{"unterminated quote", "version: 1\nkey: \"" + configCanary + "\n"},
		{"tab indent", "version: 1\nprojects:\n\t" + configCanary + ": x\n"},
		{"duplicate key", "version: 1\nprojects:\n  p:\n    environments:\n      e:\n        service: a\n        service: " + configCanary + "\n"},
	}

	dir := t.TempDir()

	for _, tc := range malformed {
		t.Run(tc.name, func(t *testing.T) {
			var errs []error

			_, err := config.Parse([]byte(tc.src))
			errs = append(errs, err)

			path := filepath.Join(dir, strings.ReplaceAll(tc.name, " ", "_")+".yaml")
			if writeErr := os.WriteFile(path, []byte(tc.src), 0o600); writeErr != nil {
				t.Fatal(writeErr)
			}
			_, loadErr := config.Load(path)
			errs = append(errs, loadErr)

			for _, e := range errs {
				if e == nil {
					t.Fatalf("expected an error for %q", tc.name)
				}
			}
			assertNoConfigCanary(t, errs...)
		})
	}
}

func assertNoConfigCanary(t *testing.T, errs ...error) {
	t.Helper()

	var rendered []string
	for _, err := range errs {
		if err == nil {
			continue
		}
		rendered = append(rendered,
			err.Error(),
			fmt.Sprintf("%v", err),
			fmt.Sprintf("%s", err),
			fmt.Sprintf("%q", err),
			fmt.Sprintf("%+v", err),
			fmt.Sprintf("%#v", err),
			fmt.Errorf("wrapped: %w", err).Error(),
			errors.Join(err, errors.New("context")).Error(),
		)
	}

	for i, got := range rendered {
		if strings.Contains(got, configCanary) {
			t.Errorf("config content leaked into an error string (rendering %d):\n%s", i, got)
		}
	}
	if strings.TrimSpace(strings.Join(rendered, "\n")) == "" {
		t.Fatal("no error text was produced at all")
	}
}
