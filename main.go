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

func main() {
	app := &cli.App{
		Stdout: os.Stdout,
		Stderr: os.Stderr,
		Stdin:  os.Stdin,
		Getenv: os.Getenv,
	}
	os.Exit(app.Run(context.Background(), os.Args[1:]))
}
