package config

import (
	"fmt"
	"os"
	"path/filepath"
)

// ExampleYAML is the starter configuration written by `renv init` and shipped
// as example.config.yaml at the repository root. A test asserts the two are
// identical, so the documented example can never drift from what init writes.
//
// Every identifier below is a placeholder. Real service, group and owner ids
// belong only in the user's own configuration file, never in the repository.
const ExampleYAML = `# renv configuration
#
# Copy to ~/.config/renv/config.yaml and replace every placeholder id.
# Run "renv services" to list the ids your API key can see.
#
# renv reads this file at runtime. It contains ids and file paths, not
# credentials: the API key is read from the environment variable named by
# defaults.api_key_env.
version: 1

defaults:
  api_base: https://api.render.com/v1
  # The API rejects a larger page size with HTTP 400.
  page_limit: 100
  # Suffix for .env backups, as a Go reference-time layout, rendered in UTC.
  backup_timestamp: 20060102T150405Z
  # Environment variable renv reads the Render API key from.
  api_key_env: RENDER_API_KEY

# Keys renv refuses to read or write in either direction, checked before
# anything else. A trailing * makes an entry a prefix match. An entry here
# cannot be re-enabled by naming the key under a project's manage list.
#
# Signing keys and the Render credential itself are the two categories that
# should never move between a local file and a service by accident.
deny_prefixes:
  - DEMO_*
  - RENDER_API_KEY

projects:
  # A project with two real environments: separate services, separate
  # databases, separate payment credentials.
  example-multi:
    environments:
      dev:
        service: srv-EXAMPLEDEVSERVICE00
        service_name: dev.example.com
        owner: tea-EXAMPLEOWNERID00000
        group: evg-EXAMPLEGROUPID00000
        env_files:
          - ~/src/example/.env.dev
        # The allowlist. Only these keys are compared or written, and each
        # declares where it lives. renv reads groups to resolve precedence
        # but never writes them, so a key homed to the group is reported
        # and never pushed.
        manage:
          APP_ENV: service
          STRIPE_TEST_SECRET_KEY: service
          EXTERNAL_DATABASE_URL: group
        # Blocked in both directions for this environment only.
        local_only:
          - LOCAL_SCRATCH_*
      prod:
        # Writing to this target additionally requires --yes-prod.
        prod: true
        service: srv-EXAMPLEPRODSERVICE0
        service_name: example.com
        owner: tea-EXAMPLEOWNERID00000
        group: evg-EXAMPLEGROUPID00000
        env_files:
          - ~/src/example/.env.prod
        manage:
          APP_ENV: service
          STRIPE_PROD_SECRET_KEY: service
          EXTERNAL_DATABASE_URL: group

  # A project with one environment whose configuration is split across two
  # complementary files. They are halves of one thing, not variants of it:
  # renv merges them, and a key present in both with different values is an
  # error rather than a last-wins guess.
  example-single:
    environments:
      default:
        service: srv-EXAMPLESINGLESVC00
        service_name: example.org
        owner: tea-EXAMPLEOWNERID00000
        env_files:
          - ~/src/example-single/.env
          - ~/src/example-single-extra/.env
        manage:
          AI_PROVIDER: service
          HEARTBEAT_URL: service
`

// InitResult describes what Init did.
type InitResult struct {
	Path    string
	Created bool
}

// Init writes the example configuration to path if nothing is there yet.
//
// An existing file is never overwritten: it holds the mapping between local
// files and live services, and replacing it with placeholders would be both
// destructive and silent.
func Init(path string) (InitResult, error) {
	if _, err := os.Stat(path); err == nil {
		return InitResult{Path: path, Created: false}, nil
	} else if !os.IsNotExist(err) {
		return InitResult{Path: path}, err
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return InitResult{Path: path}, fmt.Errorf("config: creating directory: %w", err)
	}
	if err := os.WriteFile(path, []byte(ExampleYAML), 0o600); err != nil {
		return InitResult{Path: path}, fmt.Errorf("config: writing %s: %w", path, err)
	}
	return InitResult{Path: path, Created: true}, nil
}
