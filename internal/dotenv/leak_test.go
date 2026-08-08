package dotenv_test

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pigfox/render-env-sync/internal/dotenv"
	"github.com/pigfox/render-env-sync/internal/secret"
)

// leakCanary stands in for the live credential that a real malformed line
// turned out to contain.
const leakCanary = "sk_CANARY_d97d0f3b4de13d658337f92e9275199f000ad952523e925a"

// TestParseErrorsNeverQuoteFileContent is named for the failure it prevents.
//
// A malformed .env line is, by definition, bytes the parser could not
// classify — and in the case that produced this test, an `export KEY='...'`
// line whose content was a live API key. Echoing the raw line into an error
// string put that key on a terminal, into a shell history, and into a bug
// report. Every other path in this program fingerprints values; the parser is
// the one place that handles unvalidated bytes, and so the one place the rule
// has to be enforced rather than assumed.
//
// An error may name a file, a line number, a key, or a fingerprint. It may
// never contain the line.
func TestParseErrorsNeverQuoteFileContent(t *testing.T) {
	malformed := []struct {
		name string
		src  string
	}{
		{"no separator", "GOOD=1\nthis line has no equals " + leakCanary + "\n"},
		{"invalid key characters", "GOOD=1\nBAD-KEY=" + leakCanary + "\n"},
		{"leading digit key", "GOOD=1\n1BAD=" + leakCanary + "\n"},
		{"empty key", "GOOD=1\n=" + leakCanary + "\n"},
		{"unterminated quote", "GOOD=1\nKEY=\"" + leakCanary + "\n"},
		{"duplicate differing", "K=" + leakCanary + "\nK=" + leakCanary + "-other\n"},
	}

	dir := t.TempDir()

	for _, tc := range malformed {
		t.Run(tc.name, func(t *testing.T) {
			var errs []error

			_, err := dotenv.Parse([]byte(tc.src))
			errs = append(errs, err)

			path := filepath.Join(dir, strings.ReplaceAll(tc.name, " ", "_")+".env")
			if writeErr := os.WriteFile(path, []byte(tc.src), 0o600); writeErr != nil {
				t.Fatal(writeErr)
			}
			_, fileErr := dotenv.ParseFile(path)
			errs = append(errs, fileErr)

			for _, e := range errs {
				if e == nil {
					t.Fatalf("expected an error for %q", tc.name)
				}
			}

			assertNoCanary(t, errs...)
		})
	}
}

// TestMergeAndWriteErrorsNeverQuoteValues covers the error paths that operate
// on values which did parse successfully.
func TestMergeAndWriteErrorsNeverQuoteValues(t *testing.T) {
	a, err := dotenv.Parse([]byte("SHARED=" + leakCanary + "\n"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := dotenv.Parse([]byte("SHARED=" + leakCanary + "-different\n"))
	if err != nil {
		t.Fatal(err)
	}
	_, mergeErr := dotenv.Merge([]dotenv.Source{
		{Path: "a/.env", File: a},
		{Path: "b/.env", File: b},
	}, nil)
	if mergeErr == nil {
		t.Fatal("expected a merge conflict")
	}

	f, err := dotenv.Parse([]byte("K=v\n"))
	if err != nil {
		t.Fatal(err)
	}
	f.Set("K", secret.New(`x "`+leakCanary+`" y`))
	_, writeErr := f.Bytes()
	if writeErr == nil {
		t.Fatal("expected an unquotable-value error")
	}

	assertNoCanary(t, mergeErr, writeErr)
}

// assertNoCanary formats each error every way a caller plausibly might and
// asserts the canary appears in none of them.
func assertNoCanary(t *testing.T, errs ...error) {
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
			fmt.Sprintf("%v", errors.Join(err, errors.New("context"))),
		)
	}

	for i, got := range rendered {
		if strings.Contains(got, leakCanary) {
			t.Errorf("file content leaked into an error string (rendering %d):\n%s", i, got)
		}
	}

	// Guard against passing vacuously: the errors must actually say something.
	joined := strings.Join(rendered, "\n")
	if strings.TrimSpace(joined) == "" {
		t.Fatal("no error text was produced at all")
	}
}
