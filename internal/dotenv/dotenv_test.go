package dotenv_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/pigfox/render-env-sync/internal/dotenv"
	"github.com/pigfox/render-env-sync/internal/secret"
)

// golden is the canonical form renv writes: comments and blanks preserved,
// values quoted only when they contain whitespace or '#'.
const golden = `# renv golden fixture
# comments and blank lines survive a round trip

PLAIN=value
EMPTY=
QUOTED_BECAUSE_SPACE="two words"
QUOTED_BECAUSE_HASH="has#hash"
URL=https://example.com/path?a=b
NUMERIC=42

# trailing comment block
LAST=end
`

func TestRoundTripIsByteIdentical(t *testing.T) {
	f, err := dotenv.Parse([]byte(golden))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	got, err := f.Bytes()
	if err != nil {
		t.Fatalf("Bytes: %v", err)
	}
	if string(got) != golden {
		t.Fatalf("round trip differs:\n--- got ---\n%s\n--- want ---\n%s", got, golden)
	}
}

func TestRoundTripWithoutTrailingNewline(t *testing.T) {
	const src = "A=1\nB=2"
	f, err := dotenv.Parse([]byte(src))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	got, err := f.Bytes()
	if err != nil {
		t.Fatalf("Bytes: %v", err)
	}
	if string(got) != src {
		t.Fatalf("Bytes = %q, want %q", got, src)
	}
}

func TestParseEmptyInput(t *testing.T) {
	for _, src := range []string{"", "\n"} {
		f, err := dotenv.Parse([]byte(src))
		if err != nil {
			t.Fatalf("Parse(%q): %v", src, err)
		}
		got, err := f.Bytes()
		if err != nil {
			t.Fatalf("Bytes: %v", err)
		}
		if len(f.Keys()) != 0 {
			t.Errorf("Parse(%q) produced keys %v", src, f.Keys())
		}
		if src == "" && string(got) != "" {
			t.Errorf("Bytes = %q, want empty", got)
		}
	}
}

func TestQuoteStripping(t *testing.T) {
	f, err := dotenv.Parse([]byte("D=\"double\"\nS='single'\nBARE=bare\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	for _, tc := range []struct{ key, want string }{
		{"D", "double"}, {"S", "single"}, {"BARE", "bare"},
	} {
		v, ok := f.Get(tc.key)
		if !ok {
			t.Fatalf("%s missing", tc.key)
		}
		if got := secret.Reveal(v); got != tc.want {
			t.Errorf("%s = %q, want %q", tc.key, got, tc.want)
		}
	}
}

func TestPresentEmptyVersusAbsent(t *testing.T) {
	f, err := dotenv.Parse([]byte("PRESENT=\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	v, ok := f.Get("PRESENT")
	if !ok {
		t.Fatal("PRESENT reported absent")
	}
	if !v.Empty() {
		t.Fatal("PRESENT should be empty")
	}
	if _, ok := f.Get("ABSENT"); ok {
		t.Fatal("ABSENT reported present")
	}
}

func TestMultilineValueRejected(t *testing.T) {
	for _, src := range []string{
		"KEY=\"unterminated\n",
		"KEY='unterminated\n",
		"KEY=\"\n",
	} {
		_, err := dotenv.Parse([]byte(src))
		var want *dotenv.MultilineValueError
		if !errors.As(err, &want) {
			t.Fatalf("Parse(%q) error = %v, want MultilineValueError", src, err)
		}
		if !strings.Contains(want.Error(), "KEY") {
			t.Errorf("error message lacks key: %v", want)
		}
	}
}

func TestDuplicateKeyWithDifferentValuesIsAnError(t *testing.T) {
	_, err := dotenv.Parse([]byte("K=one\nK=two\n"))
	var dup *dotenv.DuplicateKeyError
	if !errors.As(err, &dup) {
		t.Fatalf("error = %v, want DuplicateKeyError", err)
	}
	if dup.FirstLine != 1 || dup.Line != 2 || dup.Key != "K" {
		t.Errorf("unexpected error detail: %+v", dup)
	}
	if dup.FirstFP == dup.FP {
		t.Error("conflicting fingerprints should differ")
	}
	if !strings.Contains(dup.Error(), "assigned twice") {
		t.Errorf("message = %q", dup.Error())
	}
}

func TestDuplicateKeyWithIdenticalValuesIsAllowed(t *testing.T) {
	f, err := dotenv.Parse([]byte("K=same\nK=same\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if got := f.Keys(); len(got) != 2 {
		t.Fatalf("Keys = %v, want both lines retained", got)
	}
}

func TestSyntaxErrors(t *testing.T) {
	for _, src := range []string{
		"export FOO=bar\n",
		"not an assignment\n",
		"1BAD=x\n",
		"=novalue\n",
		"BAD-KEY=x\n",
	} {
		_, err := dotenv.Parse([]byte(src))
		var se *dotenv.SyntaxError
		if !errors.As(err, &se) {
			t.Errorf("Parse(%q) error = %v, want SyntaxError", src, err)
			continue
		}
		if !strings.Contains(se.Error(), "KEY=VALUE") {
			t.Errorf("message = %q", se.Error())
		}
	}
}

func TestUnquotableValue(t *testing.T) {
	f, err := dotenv.Parse([]byte("K=v\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	f.Set("K", secret.New(`a "quoted" word`))
	_, err = f.Bytes()
	var ue *dotenv.UnquotableValueError
	if !errors.As(err, &ue) {
		t.Fatalf("error = %v, want UnquotableValueError", err)
	}
	if !strings.Contains(ue.Error(), "K") {
		t.Errorf("message = %q", ue.Error())
	}
}

func TestSetUpdatesInPlaceAndAppends(t *testing.T) {
	f, err := dotenv.Parse([]byte("# lead\nA=1\nB=2\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	f.Set("A", secret.New("updated"))
	f.Set("C", secret.New("new"))

	got, err := f.Bytes()
	if err != nil {
		t.Fatalf("Bytes: %v", err)
	}
	const want = "# lead\nA=updated\nB=2\nC=new\n"
	if string(got) != want {
		t.Fatalf("Bytes =\n%q\nwant\n%q", got, want)
	}
}

func TestSetOnEmptyFileAppends(t *testing.T) {
	f, err := dotenv.Parse([]byte(""))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	f.Set("A", secret.New("1"))
	got, err := f.Bytes()
	if err != nil {
		t.Fatalf("Bytes: %v", err)
	}
	if string(got) != "A=1\n" {
		t.Fatalf("Bytes = %q", got)
	}
}

func TestDelete(t *testing.T) {
	f, err := dotenv.Parse([]byte("A=1\nB=2\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if !f.Delete("A") {
		t.Fatal("Delete(A) reported nothing removed")
	}
	if f.Delete("MISSING") {
		t.Fatal("Delete(MISSING) reported a removal")
	}
	got, err := f.Bytes()
	if err != nil {
		t.Fatalf("Bytes: %v", err)
	}
	if string(got) != "B=2\n" {
		t.Fatalf("Bytes = %q", got)
	}
}

func TestKeysInFileOrder(t *testing.T) {
	f, err := dotenv.Parse([]byte("Z=1\n# c\nA=2\n\nM=3\n"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	want := []string{"Z", "A", "M"}
	got := f.Keys()
	if len(got) != len(want) {
		t.Fatalf("Keys = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Keys = %v, want %v", got, want)
		}
	}
}

func TestParseFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	if err := os.WriteFile(path, []byte(golden), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := dotenv.ParseFile(path); err != nil {
		t.Fatalf("ParseFile: %v", err)
	}

	if _, err := dotenv.ParseFile(filepath.Join(dir, "missing")); err == nil {
		t.Fatal("expected error for missing file")
	}

	bad := filepath.Join(dir, "bad.env")
	if err := os.WriteFile(bad, []byte("garbage line\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := dotenvParseFileErr(t, bad)
	if !strings.Contains(err.Error(), "bad.env") {
		t.Errorf("error should name the file: %v", err)
	}
}

func dotenvParseFileErr(t *testing.T, path string) error {
	t.Helper()
	_, err := dotenv.ParseFile(path)
	if err == nil {
		t.Fatal("expected error")
	}
	return err
}

func TestWriteAtomicCreatesBackupAndPreservesMode(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	if err := os.WriteFile(path, []byte("A=old\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	f, err := dotenv.Parse([]byte("A=new\n"))
	if err != nil {
		t.Fatal(err)
	}
	fixed := time.Date(2026, 8, 7, 12, 34, 56, 0, time.UTC)
	opt := dotenv.WriteOptions{Now: func() time.Time { return fixed }}
	if err := f.WriteAtomic(path, opt); err != nil {
		t.Fatalf("WriteAtomic: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "A=new\n" {
		t.Fatalf("file = %q", got)
	}

	backup := path + ".bak.20260807T123456Z"
	b, err := os.ReadFile(backup)
	if err != nil {
		t.Fatalf("backup missing: %v", err)
	}
	if string(b) != "A=old\n" {
		t.Fatalf("backup = %q", b)
	}

	for _, p := range []string{path, backup} {
		st, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if st.Mode().Perm() != 0o600 {
			t.Errorf("%s mode = %v, want 0600", p, st.Mode().Perm())
		}
	}
}

func TestWriteAtomicWithoutExistingFileSkipsBackup(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")

	f, err := dotenv.Parse([]byte("A=1\n"))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.WriteAtomic(path, dotenv.WriteOptions{}); err != nil {
		t.Fatalf("WriteAtomic: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected only the written file, got %d entries", len(entries))
	}
}

func TestWriteAtomicCustomOptions(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	if err := os.WriteFile(path, []byte("A=old\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := dotenv.Parse([]byte("A=new\n"))
	if err != nil {
		t.Fatal(err)
	}
	fixed := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	opt := dotenv.WriteOptions{
		BackupLayout: "2006-01-02",
		Now:          func() time.Time { return fixed },
		Perm:         0o640,
	}
	if err := f.WriteAtomic(path, opt); err != nil {
		t.Fatalf("WriteAtomic: %v", err)
	}
	if _, err := os.Stat(path + ".bak.2026-01-02"); err != nil {
		t.Fatalf("custom-layout backup missing: %v", err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o640 {
		t.Errorf("mode = %v, want 0640", st.Mode().Perm())
	}
}

func TestWriteAtomicRefusesUnrenderableFileBeforeTouchingDisk(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	if err := os.WriteFile(path, []byte("A=old\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := dotenv.Parse([]byte("A=old\n"))
	if err != nil {
		t.Fatal(err)
	}
	f.Set("A", secret.New(`bad "value"`))

	if err := f.WriteAtomic(path, dotenv.WriteOptions{}); err == nil {
		t.Fatal("expected render error")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "A=old\n" {
		t.Fatalf("original file was modified: %q", got)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("a backup or temp file was left behind: %d entries", len(entries))
	}
}

func TestWriteAtomicBackupReadError(t *testing.T) {
	dir := t.TempDir()
	// A directory at the target path makes ReadFile fail with something
	// other than not-exist, exercising the non-recoverable backup path.
	path := filepath.Join(dir, "target")
	if err := os.Mkdir(path, 0o700); err != nil {
		t.Fatal(err)
	}
	f, err := dotenv.Parse([]byte("A=1\n"))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.WriteAtomic(path, dotenv.WriteOptions{}); err == nil {
		t.Fatal("expected error backing up a directory")
	}
}

func TestWriteAtomicUnwritableDirectory(t *testing.T) {
	dir := t.TempDir()
	sub := filepath.Join(dir, "ro")
	if err := os.Mkdir(sub, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(sub, 0o700) })

	f, err := dotenv.Parse([]byte("A=1\n"))
	if err != nil {
		t.Fatal(err)
	}
	if err := f.WriteAtomic(filepath.Join(sub, ".env"), dotenv.WriteOptions{}); err == nil {
		t.Skip("running as a user that can write to a read-only directory")
	}
}

func TestMerge(t *testing.T) {
	a, err := dotenv.Parse([]byte("# comments and blanks are skipped by Merge\n\nSHARED=same\nONLY_A=1\n"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := dotenv.Parse([]byte("SHARED=same\nONLY_B=2\n"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := dotenv.Merge([]dotenv.Source{{Path: "a", File: a}, {Path: "b", File: b}})
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("merged %d keys, want 3", len(got))
	}
	if secret.Reveal(got["ONLY_B"]) != "2" {
		t.Errorf("ONLY_B = %q", secret.Reveal(got["ONLY_B"]))
	}
}

// TestMergeConflict pins the VENDOR_API_KEY case from recon: the same key in
// two complementary files with different values, where the correct survivor
// was in the later file and no fixed precedence rule could have known that.
func TestMergeConflict(t *testing.T) {
	a, err := dotenv.Parse([]byte("VENDOR_API_KEY=stale\n"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := dotenv.Parse([]byte("VENDOR_API_KEY=current\n"))
	if err != nil {
		t.Fatal(err)
	}
	_, err = dotenv.Merge([]dotenv.Source{
		{Path: "app/.env", File: a},
		{Path: "app-extra/.env", File: b},
	})
	var ce *dotenv.ConflictError
	if !errors.As(err, &ce) {
		t.Fatalf("error = %v, want ConflictError", err)
	}
	if ce.FirstPath != "app/.env" || ce.Path != "app-extra/.env" {
		t.Errorf("paths = %q, %q", ce.FirstPath, ce.Path)
	}
	if !strings.Contains(ce.Error(), "rather than relying on file order") {
		t.Errorf("message = %q", ce.Error())
	}
}
