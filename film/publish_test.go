package film

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestPublishReplacesWholeFile(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.mp4")
	dst := filepath.Join(dir, "out.mp4")
	if err := os.WriteFile(src, []byte("complete-new-film"), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, []byte("previous-valid-output"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := publish(context.Background(), src, dst); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "complete-new-film" {
		t.Fatalf("dest = %q", got)
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Fatalf("source still present: %v", err)
	}
}

func TestPublishCancelDoesNotReplace(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.mp4")
	dst := filepath.Join(dir, "out.mp4")
	if err := os.WriteFile(src, []byte("complete-new-film"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, []byte("previous-valid-output"), 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := publish(ctx, src, dst)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "previous-valid-output" {
		t.Fatalf("dest = %q", got)
	}
	if _, err := os.Stat(src); err != nil {
		t.Fatalf("source removed on cancel: %v", err)
	}
}

func TestPublishReaderFailureLeavesPreviousOutput(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "out.mp4")
	old := []byte("previous-valid-output")
	if err := os.WriteFile(dst, old, 0o644); err != nil {
		t.Fatal(err)
	}
	err := publishReader(context.Background(), &shortReader{n: 3, err: errors.New("short copy")}, 0o640, dst)
	if err == nil {
		t.Fatal("expected copy failure")
	}
	assertOutputIntact(t, dir, dst, old)
}

func TestPublishReaderCancelLeavesPreviousOutput(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "out.mp4")
	old := []byte("previous-valid-output")
	if err := os.WriteFile(dst, old, 0o644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	err := publishReader(ctx, &cancelOnEOF{cancel: cancel}, 0o644, dst)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v", err)
	}
	assertOutputIntact(t, dir, dst, old)
}

func TestPublishBesideRejectsNonRegularAndKeepsDest(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "out.mp4")
	old := []byte("previous-valid-output")
	if err := os.WriteFile(dst, old, 0o644); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(dir, "srcdir")
	if err := os.Mkdir(src, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := publishBeside(context.Background(), src, dst); err == nil {
		t.Fatal("expected non-regular source to fail")
	}
	assertOutputIntact(t, dir, dst, old)
}

func TestPublishAcrossDevicesReplacesACompleteFile(t *testing.T) {
	srcDir := t.TempDir()
	src := filepath.Join(srcDir, "src.mp4")
	body := []byte("complete-new-film")
	if err := os.WriteFile(src, body, 0o640); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(src)
	if err != nil {
		t.Fatal(err)
	}
	dstDir, err := os.MkdirTemp("/dev/shm", "keel-film-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dstDir) })
	dst := filepath.Join(dstDir, "out.mp4")
	if err := os.WriteFile(dst, []byte("previous-valid-output"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := publish(context.Background(), src, dst); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(body) {
		t.Fatalf("dest = %q", got)
	}
	if _, err := os.Stat(src); !os.IsNotExist(err) {
		t.Fatalf("source still present: %v", err)
	}
	gotInfo, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if gotInfo.Mode().Perm() != info.Mode().Perm() {
		t.Fatalf("mode = %o, want %o", gotInfo.Mode().Perm(), info.Mode().Perm())
	}
	assertNoPartial(t, dstDir)
}

func TestConcurrentPublishKeepsAWholeFile(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "out.mp4")
	if err := os.WriteFile(dst, []byte("previous-valid-output"), 0o644); err != nil {
		t.Fatal(err)
	}
	const n = 16
	bodies := make([][]byte, n)
	var wg sync.WaitGroup
	errCh := make(chan error, n)
	for i := 0; i < n; i++ {
		body := bytes.Repeat([]byte{byte('A' + i)}, 64)
		bodies[i] = body
		src := filepath.Join(dir, fmt.Sprintf("src-%02d.mp4", i))
		if err := os.WriteFile(src, body, 0o644); err != nil {
			t.Fatal(err)
		}
		wg.Add(1)
		go func(src string) {
			defer wg.Done()
			errCh <- publish(context.Background(), src, dst)
		}(src)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatal(err)
		}
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if !wholePayload(got, bodies) {
		t.Fatalf("torn or unexpected output %q", got)
	}
	assertNoPartial(t, dir)
}

func TestConcurrentPublishBesideKeepsAWholeFile(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "out.mp4")
	if err := os.WriteFile(dst, []byte("previous-valid-output"), 0o644); err != nil {
		t.Fatal(err)
	}
	const n = 16
	bodies := make([][]byte, n)
	var wg sync.WaitGroup
	errCh := make(chan error, n)
	for i := 0; i < n; i++ {
		body := bytes.Repeat([]byte{byte('A' + i)}, 64)
		bodies[i] = body
		src := filepath.Join(dir, fmt.Sprintf("src-%02d.mp4", i))
		if err := os.WriteFile(src, body, 0o644); err != nil {
			t.Fatal(err)
		}
		wg.Add(1)
		go func(src string) {
			defer wg.Done()
			errCh <- publishBeside(context.Background(), src, dst)
		}(src)
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Fatal(err)
		}
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if !wholePayload(got, bodies) {
		t.Fatalf("torn or unexpected output %q", got)
	}
	assertNoPartial(t, dir)
}

func assertOutputIntact(t *testing.T, dir, dst string, old []byte) {
	t.Helper()
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(old) {
		t.Fatalf("dest = %q", got)
	}
	assertNoPartial(t, dir)
}

func assertNoPartial(t *testing.T, dir string) {
	t.Helper()
	matches, err := filepath.Glob(filepath.Join(dir, ".keel-film-*"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Fatalf("partial files left behind: %v", matches)
	}
}

func wholePayload(got []byte, bodies [][]byte) bool {
	for _, body := range bodies {
		if bytes.Equal(got, body) {
			return true
		}
	}
	return false
}

// shortReader returns n bytes and then err.
type shortReader struct {
	n   int
	err error
}

func (r *shortReader) Read(p []byte) (int, error) {
	if r.n <= 0 {
		return 0, r.err
	}
	if len(p) == 0 {
		return 0, nil
	}
	p[0] = 'X'
	r.n--
	if r.n == 0 {
		return 1, r.err
	}
	return 1, nil
}

// cancelOnEOF yields one byte, cancels, and reports EOF so the copy finishes
// and the publish must still refuse the rename.
type cancelOnEOF struct {
	cancel context.CancelFunc
	done   bool
}

func (r *cancelOnEOF) Read(p []byte) (int, error) {
	if r.done {
		return 0, io.EOF
	}
	r.done = true
	if len(p) == 0 {
		return 0, nil
	}
	p[0] = 'Z'
	r.cancel()
	return 1, io.EOF
}
