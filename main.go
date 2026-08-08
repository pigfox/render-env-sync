// Command renv keeps local .env files and Render service configuration in
// step, without ever issuing the API call that would delete the difference.
//
// See the README for the safety model. This file is dispatch only; the
// commands live in internal/cli.
package main

import (
	"context"
	"os"

	"github.com/pigfox/render-env-sync/internal/cli"
)

// version is injected at build time with
//
//	go build -ldflags "-X main.version=v1.2.3"
//
// It stays "dev" for a plain `go build` or `go install` from source, which is
// the honest answer: such a binary corresponds to no released artifact.
var version = "dev"

func main() {
	app := &cli.App{
		Version: version,
		Stdout:  os.Stdout,
		Stderr:  os.Stderr,
		Stdin:   os.Stdin,
		Getenv:  os.Getenv,
	}
	os.Exit(app.Run(context.Background(), os.Args[1:]))
}
