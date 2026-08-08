// Package dotenv is a line-oriented .env reader and writer.
//
// The parser keeps every line — blanks and comments included — so that a file
// can be read, modified in one place, and written back with everything else
// byte-identical. A .env file is human-edited configuration, and a tool that
// reformats it on every write makes its own diffs unreadable.
//
// Three behaviours are deliberate refusals rather than conveniences:
//
//   - A value that opens a quote and does not close it on the same line is an
//     error. Truncating at the newline would silently install half a secret.
//   - The same key twice in one file with different values is an error.
//     Last-wins is a guess, and PF-S80 recon found a real case where the
//     stale value came first.
//   - A line that is neither blank, comment, nor assignment is an error.
//
// Errors from this package name a file, a line, a key, or a fingerprint. They
// never quote the line. A malformed line is by definition bytes the parser
// could not classify, and in the case that motivated this rule the bytes were
// a live API key: echoing them into an error string put that key on a
// terminal, into shell history, and into a bug report.
package dotenv

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pigfox/render-env-sync/internal/secret"
)

// Kind classifies a line of a .env file.
type Kind int

const (
	// KindBlank is a line containing only whitespace.
	KindBlank Kind = iota
	// KindComment is a line whose first non-space character is '#'.
	KindComment
	// KindAssign is a KEY=VALUE line.
	KindAssign
)

// Line is one line of a .env file. For KindBlank and KindComment the original
// text is kept verbatim in Text; for KindAssign the parsed Key and Value are
// authoritative and Text is unused.
type Line struct {
	Kind  Kind
	Text  string
	Key   string
	Value secret.Secret

	// Indent is the leading whitespace of an assignment, and Export records
	// a leading "export ". Both are kept so that a file using either style
	// writes back byte-identically.
	Indent string
	Export bool
}

// File is a parsed .env file.
type File struct {
	Lines []Line

	// trailingNewline records whether the source ended with a newline, so
	// that writing back does not add or drop one.
	trailingNewline bool
}

// SyntaxProblem classifies why a line could not be parsed.
type SyntaxProblem string

const (
	// ProblemNoSeparator means the line has no "=" at all.
	ProblemNoSeparator SyntaxProblem = "not a comment or KEY=VALUE assignment (no '=' separator)"
	// ProblemEmptyKey means the line begins with "=".
	ProblemEmptyKey SyntaxProblem = "empty key before '='"
	// ProblemInvalidKey means the text before "=" is not a shell identifier.
	ProblemInvalidKey SyntaxProblem = "invalid characters in key; expected letters, digits and underscore"
)

// SyntaxError reports a line that is not blank, a comment, or an assignment.
//
// It carries no field holding the line, and none naming a key either. A syntax
// error is exactly the case where no valid identifier could be parsed, so the
// candidate key is itself untrusted bytes — for a stray base64 or token line
// it would be part of the secret. Where a key *can* be parsed and the value is
// the problem, [MultilineValueError] names it.
type SyntaxError struct {
	Line    int
	Problem SyntaxProblem
}

func (e *SyntaxError) Error() string {
	return fmt.Sprintf("line %d: %s", e.Line, e.Problem)
}

// MultilineValueError reports a value whose opening quote is never closed on
// the same line.
type MultilineValueError struct {
	Line int
	Key  string
}

func (e *MultilineValueError) Error() string {
	return fmt.Sprintf("line %d: %s: unterminated quoted value; multiline values are not supported", e.Line, e.Key)
}

// DuplicateKeyError reports the same key assigned twice with different values
// inside a single file.
type DuplicateKeyError struct {
	Key       string
	FirstLine int
	Line      int
	FirstFP   string
	FP        string
}

func (e *DuplicateKeyError) Error() string {
	return fmt.Sprintf("%s assigned twice with different values: line %d (%s) and line %d (%s)",
		e.Key, e.FirstLine, e.FirstFP, e.Line, e.FP)
}

// UnquotableValueError reports a value that needs quoting to survive a write
// but contains a double quote of its own.
type UnquotableValueError struct {
	Key string
}

func (e *UnquotableValueError) Error() string {
	return fmt.Sprintf("%s: value needs quoting but contains a double quote; renv will not guess an escaping", e.Key)
}

// Parse reads a .env file from src.
func Parse(src []byte) (*File, error) {
	text := string(src)
	f := &File{}
	if strings.HasSuffix(text, "\n") {
		f.trailingNewline = true
		text = strings.TrimSuffix(text, "\n")
	}

	// An empty source has no lines at all. A source of just "\n" has one
	// blank line, and must keep it to write back byte-identically.
	var raw []string
	if len(src) > 0 {
		raw = strings.Split(text, "\n")
	}

	type seen struct {
		line int
		fp   string
	}
	first := map[string]seen{}

	for i, rawLine := range raw {
		lineNo := i + 1
		trimmed := strings.TrimSpace(rawLine)

		switch {
		case trimmed == "":
			f.Lines = append(f.Lines, Line{Kind: KindBlank, Text: rawLine})
			continue
		case strings.HasPrefix(trimmed, "#"):
			f.Lines = append(f.Lines, Line{Kind: KindComment, Text: rawLine})
			continue
		}

		// Leading whitespace and an "export " prefix are both accepted and
		// both recorded, so that a file written in either style survives a
		// round trip unchanged.
		body := strings.TrimLeft(rawLine, " \t")
		indent := rawLine[:len(rawLine)-len(body)]
		export := false
		if rest, found := strings.CutPrefix(body, "export "); found {
			export = true
			body = strings.TrimLeft(rest, " \t")
		}

		key, rawValue, ok := strings.Cut(body, "=")
		switch {
		case !ok:
			return nil, &SyntaxError{Line: lineNo, Problem: ProblemNoSeparator}
		case key == "":
			return nil, &SyntaxError{Line: lineNo, Problem: ProblemEmptyKey}
		case !validKey(key):
			return nil, &SyntaxError{Line: lineNo, Problem: ProblemInvalidKey}
		}

		value, err := unquote(rawValue)
		if err != nil {
			return nil, &MultilineValueError{Line: lineNo, Key: key}
		}

		v := secret.New(value)
		if prev, dup := first[key]; dup && prev.fp != v.Fingerprint() {
			return nil, &DuplicateKeyError{
				Key: key, FirstLine: prev.line, Line: lineNo,
				FirstFP: prev.fp, FP: v.Fingerprint(),
			}
		} else if !dup {
			first[key] = seen{line: lineNo, fp: v.Fingerprint()}
		}

		f.Lines = append(f.Lines, Line{
			Kind: KindAssign, Key: key, Value: v, Indent: indent, Export: export,
		})
	}
	return f, nil
}

// ParseFile reads and parses the file at path.
func ParseFile(path string) (*File, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	f, err := Parse(b)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return f, nil
}

// validKey reports whether s is a shell-style identifier.
func validKey(s string) bool {
	if s == "" {
		return false
	}
	for i, r := range s {
		switch {
		case r == '_':
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z':
		case r >= '0' && r <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
}

// unquote strips one layer of matching surrounding quotes. It reports an error
// when a quote is opened and not closed, which in a line-oriented format means
// the value continued onto another line.
func unquote(s string) (string, error) {
	if s == "" {
		return "", nil
	}
	q := s[0]
	if q != '"' && q != '\'' {
		return s, nil
	}
	if len(s) < 2 || s[len(s)-1] != q {
		return "", fmt.Errorf("unterminated quote")
	}
	return s[1 : len(s)-1], nil
}

// needsQuote reports whether a value must be quoted to survive a round trip.
func needsQuote(v string) bool {
	return strings.ContainsAny(v, " \t#")
}

// Get returns the value for key. The second result distinguishes a key that is
// present with an empty value (FOO=) from one that is absent.
func (f *File) Get(key string) (secret.Secret, bool) {
	for _, l := range f.Lines {
		if l.Kind == KindAssign && l.Key == key {
			return l.Value, true
		}
	}
	return secret.Secret{}, false
}

// Keys returns the assigned keys in file order.
func (f *File) Keys() []string {
	var out []string
	for _, l := range f.Lines {
		if l.Kind == KindAssign {
			out = append(out, l.Key)
		}
	}
	return out
}

// Set updates key in place if present, and otherwise appends a new assignment
// at the end of the file.
func (f *File) Set(key string, v secret.Secret) {
	for i, l := range f.Lines {
		if l.Kind == KindAssign && l.Key == key {
			f.Lines[i].Value = v
			return
		}
	}
	f.Lines = append(f.Lines, Line{Kind: KindAssign, Key: key, Value: v})
	f.trailingNewline = true
}

// Delete removes the assignment for key. It reports whether a line was
// removed.
func (f *File) Delete(key string) bool {
	for i, l := range f.Lines {
		if l.Kind == KindAssign && l.Key == key {
			f.Lines = append(f.Lines[:i], f.Lines[i+1:]...)
			return true
		}
	}
	return false
}

// Bytes renders the file.
//
// This is the .env writer: one of the two places permitted to call
// [secret.Reveal].
func (f *File) Bytes() ([]byte, error) {
	var b strings.Builder
	for i, l := range f.Lines {
		if i > 0 {
			b.WriteByte('\n')
		}
		switch l.Kind {
		case KindAssign:
			plain := secret.Reveal(l.Value)
			b.WriteString(l.Indent)
			if l.Export {
				b.WriteString("export ")
			}
			if needsQuote(plain) {
				if strings.Contains(plain, `"`) {
					return nil, &UnquotableValueError{Key: l.Key}
				}
				b.WriteString(l.Key + `="` + plain + `"`)
			} else {
				b.WriteString(l.Key + "=" + plain)
			}
		default:
			b.WriteString(l.Text)
		}
	}
	if f.trailingNewline && len(f.Lines) > 0 {
		b.WriteByte('\n')
	}
	return []byte(b.String()), nil
}

// WriteOptions tunes [File.WriteAtomic]. The zero value is usable and applies
// the defaults below.
type WriteOptions struct {
	// BackupLayout is a Go reference-time layout used for the backup file
	// suffix, rendered in UTC. Defaults to DefaultBackupLayout.
	BackupLayout string
	// Now supplies the backup timestamp. Defaults to time.Now.
	Now func() time.Time
	// Perm is the mode for both the backup and the written file. Defaults
	// to 0600, because these files hold credentials.
	Perm os.FileMode
}

// DefaultBackupLayout is the UTC timestamp layout appended to backups.
const DefaultBackupLayout = "20060102T150405Z"

func (o WriteOptions) withDefaults() WriteOptions {
	if o.BackupLayout == "" {
		o.BackupLayout = DefaultBackupLayout
	}
	if o.Now == nil {
		o.Now = time.Now
	}
	if o.Perm == 0 {
		o.Perm = 0o600
	}
	return o
}

// WriteAtomic backs up any existing file to <path>.bak.<utc-timestamp>, then
// writes the rendered file to a temporary file in the same directory and
// renames it over path.
//
// Rendering happens before the backup, so a value that cannot be written
// leaves the original file and its backups untouched.
func (f *File) WriteAtomic(path string, opt WriteOptions) error {
	opt = opt.withDefaults()

	data, err := f.Bytes()
	if err != nil {
		return err
	}

	if err := f.backup(path, opt); err != nil {
		return err
	}

	dir := filepath.Dir(path)
	tmp, err := createTemp(dir, "."+filepath.Base(path)+".renv-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if err := tmp.Chmod(opt.Perm); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return rename(tmpName, path)
}

// tempFile is the slice of *os.File that WriteAtomic uses. Naming it lets the
// tests drive the failure paths of a partially written credential file, which
// is the case that most needs to leave the original intact and is otherwise
// only reachable by filling a disk.
type tempFile interface {
	Write([]byte) (int, error)
	Close() error
	Chmod(os.FileMode) error
	Name() string
}

var (
	createTemp = func(dir, pattern string) (tempFile, error) { return os.CreateTemp(dir, pattern) }
	rename     = os.Rename
)

// backup copies an existing file aside. A missing file is not an error: the
// first write of a new .env has nothing to preserve.
func (f *File) backup(path string, opt WriteOptions) error {
	existing, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	stamp := opt.Now().UTC().Format(opt.BackupLayout)
	return os.WriteFile(path+".bak."+stamp, existing, opt.Perm)
}

// Source pairs a parsed file with the path it came from, so that conflicts can
// name their origin.
type Source struct {
	Path string
	File *File
}

// ConflictError reports the same key assigned different values in two files.
type ConflictError struct {
	Key       string
	FirstPath string
	Path      string
	FirstFP   string
	FP        string
}

func (e *ConflictError) Error() string {
	return fmt.Sprintf("%s defined differently in %s (%s) and %s (%s); resolve it rather than relying on file order",
		e.Key, e.FirstPath, e.FirstFP, e.Path, e.FP)
}

// Merge flattens several files into one key set.
//
// skip, when non-nil, drops a key from every file before comparison. Keys the
// manifest blocks in both directions must not be able to fail a command via a
// conflict between two files neither of which renv would ever read or write.
//
// A key repeated across files with the same value is fine — the estate has
// complementary halves that legitimately overlap. A key repeated with
// different values is a [ConflictError] rather than a silent last-wins,
// because recon found exactly one such case and the correct survivor was the
// value in the later file, which no fixed precedence rule could have known.
func Merge(sources []Source, skip func(key string) bool) (map[string]secret.Secret, error) {
	out := map[string]secret.Secret{}
	origin := map[string]string{}

	for _, src := range sources {
		for _, l := range src.File.Lines {
			if l.Kind != KindAssign {
				continue
			}
			if skip != nil && skip(l.Key) {
				continue
			}
			if prev, ok := out[l.Key]; ok && !prev.Equal(l.Value) {
				return nil, &ConflictError{
					Key:       l.Key,
					FirstPath: origin[l.Key],
					Path:      src.Path,
					FirstFP:   prev.Fingerprint(),
					FP:        l.Value.Fingerprint(),
				}
			}
			out[l.Key] = l.Value
			origin[l.Key] = src.Path
		}
	}
	return out, nil
}
