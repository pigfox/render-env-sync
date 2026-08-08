package dotenv

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// fakeTemp drives the failure paths of the atomic write. Each of these is a
// real way to leave a half-written credential file on disk, and none of them
// is reachable from an external test without filling a disk or revoking
// permissions mid-write.
type fakeTemp struct {
	name     string
	chmodErr error
	writeErr error
	closeErr error
	closed   bool
}

func (f *fakeTemp) Write(p []byte) (int, error) {
	if f.writeErr != nil {
		return 0, f.writeErr
	}
	return len(p), nil
}
func (f *fakeTemp) Close() error            { f.closed = true; return f.closeErr }
func (f *fakeTemp) Chmod(os.FileMode) error { return f.chmodErr }
func (f *fakeTemp) Name() string            { return f.name }

func withFakeTemp(t *testing.T, ft *fakeTemp) {
	t.Helper()
	prev := createTemp
	createTemp = func(dir, pattern string) (tempFile, error) {
		ft.name = filepath.Join(dir, "fake-temp")
		return ft, nil
	}
	t.Cleanup(func() { createTemp = prev })
}

func newFile(t *testing.T) *File {
	t.Helper()
	f, err := Parse([]byte("A=1\n"))
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func TestWriteAtomicChmodError(t *testing.T) {
	want := errors.New("chmod boom")
	ft := &fakeTemp{chmodErr: want}
	withFakeTemp(t, ft)

	err := newFile(t).WriteAtomic(filepath.Join(t.TempDir(), ".env"), WriteOptions{})
	if !errors.Is(err, want) {
		t.Fatalf("err = %v, want %v", err, want)
	}
	if !ft.closed {
		t.Error("temp file was not closed after a chmod failure")
	}
}

func TestWriteAtomicWriteError(t *testing.T) {
	want := errors.New("write boom")
	ft := &fakeTemp{writeErr: want}
	withFakeTemp(t, ft)

	err := newFile(t).WriteAtomic(filepath.Join(t.TempDir(), ".env"), WriteOptions{})
	if !errors.Is(err, want) {
		t.Fatalf("err = %v, want %v", err, want)
	}
	if !ft.closed {
		t.Error("temp file was not closed after a write failure")
	}
}

func TestWriteAtomicCloseError(t *testing.T) {
	want := errors.New("close boom")
	withFakeTemp(t, &fakeTemp{closeErr: want})

	err := newFile(t).WriteAtomic(filepath.Join(t.TempDir(), ".env"), WriteOptions{})
	if !errors.Is(err, want) {
		t.Fatalf("err = %v, want %v", err, want)
	}
}

func TestWriteAtomicRenameError(t *testing.T) {
	want := errors.New("rename boom")
	withFakeTemp(t, &fakeTemp{})

	prev := rename
	rename = func(string, string) error { return want }
	t.Cleanup(func() { rename = prev })

	err := newFile(t).WriteAtomic(filepath.Join(t.TempDir(), ".env"), WriteOptions{})
	if !errors.Is(err, want) {
		t.Fatalf("err = %v, want %v", err, want)
	}
}

func TestWriteAtomicCreateTempError(t *testing.T) {
	want := errors.New("create boom")
	prev := createTemp
	createTemp = func(string, string) (tempFile, error) { return nil, want }
	t.Cleanup(func() { createTemp = prev })

	err := newFile(t).WriteAtomic(filepath.Join(t.TempDir(), ".env"), WriteOptions{})
	if !errors.Is(err, want) {
		t.Fatalf("err = %v, want %v", err, want)
	}
}

// TestValidKey pins the identifier rule directly. Parse classifies an empty
// key separately so it never reaches validKey with "", but the helper is the
// definition of what renv accepts as a key and its contract is worth stating.
func TestValidKey(t *testing.T) {
	valid := []string{"A", "_", "_A", "KEY", "KEY_1", "k9", "DD_SLOT0_MODEL_ID"}
	for _, s := range valid {
		if !validKey(s) {
			t.Errorf("validKey(%q) = false, want true", s)
		}
	}
	invalid := []string{"", "1KEY", "9", "BAD-KEY", "BAD KEY", "KEY.SUB", "KEY=X", "é"}
	for _, s := range invalid {
		if validKey(s) {
			t.Errorf("validKey(%q) = true, want false", s)
		}
	}
}
