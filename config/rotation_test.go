package config

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// probeCell holds one mutable value behind a mutex.
type probeCell struct {
	mu    sync.Mutex
	value string
	empty bool
}

func (c *probeCell) get() (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.value, !c.empty
}

func (c *probeCell) set(value string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.value = value
	c.empty = false
}

func (c *probeCell) clear() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.value = ""
	c.empty = true
}

// probeSource resolves from a probeCell. It names misses
// without carrying a value.
type probeSource struct {
	cell *probeCell
}

func (s probeSource) Resolve(ctx context.Context, ref Ref) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	val, ok := s.cell.get()
	if !ok {
		return "", errors.New("probe: backing store is empty")
	}
	return val, nil
}

func writeDoc(t *testing.T, doc string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "app.toml")
	if err := os.WriteFile(path, []byte(doc), 0o600); err != nil {
		t.Fatalf("write settings: %v", err)
	}
	return path
}

type livePair struct {
	Boot Secret `toml:"boot"`
	Live Secret `toml:"live"`
}

type liveSingle struct {
	Live Secret `toml:"live"`
}

func liveRegistry(cells map[string]*probeCell) *Registry {
	reg := NewRegistry()
	for name, cell := range cells {
		src := probeSource{cell: cell}
		if err := reg.Register(name, src); err != nil {
			panic(err)
		}
	}
	return reg
}

func TestLiveTracksRotation(t *testing.T) {
	boot := &probeCell{value: "boot-one"}
	live := &probeCell{value: "live-one"}
	reg := liveRegistry(map[string]*probeCell{"boot": boot, "live": live})
	doc := "[boot]\nsource = \"boot\"\nvar = \"BOOT_KEY\"\n[live]\nsource = \"live\"\nvar = \"LIVE_KEY\"\nread = \"at_use\"\n"
	var got livePair
	if _, err := Load(t.Context(), &got, Config{
		Path:      writeDoc(t, doc),
		LookupEnv: mapLookup(nil),
		Registry:  reg,
	}); err != nil {
		t.Fatalf("Load: %v", err)
	}
	boot.set("boot-two")
	live.set("live-two")
	bootVal, err := got.Boot.Reveal()
	if err != nil {
		t.Fatalf("boot Reveal: %v", err)
	}
	if bootVal != "boot-one" {
		t.Errorf("at_boot Reveal = %q, want boot-one", bootVal)
	}
	liveVal, err := got.Live.Reveal()
	if err != nil {
		t.Fatalf("live Reveal: %v", err)
	}
	if liveVal != "live-two" {
		t.Errorf("at_use Reveal = %q, want live-two", liveVal)
	}
}

func TestLiveWithoutStaleValue(t *testing.T) {
	live := &probeCell{value: "live-one"}
	reg := liveRegistry(map[string]*probeCell{"live": live})
	doc := "[live]\nsource = \"live\"\nvar = \"LIVE_KEY\"\nread = \"at_use\"\n"
	var got liveSingle
	if _, err := Load(t.Context(), &got, Config{
		Path:      writeDoc(t, doc),
		LookupEnv: mapLookup(nil),
		Registry:  reg,
	}); err != nil {
		t.Fatalf("Load: %v", err)
	}
	live.clear()
	if _, err := got.Live.Reveal(); err == nil {
		t.Fatalf("Reveal after delete succeeded, want failure")
	} else {
		if !errors.Is(err, ErrResolve) {
			t.Errorf("err = %v, want ErrResolve", err)
		}
		msg := err.Error()
		for _, part := range []string{"live", "source live", "locator var=LIVE_KEY"} {
			if !strings.Contains(msg, part) {
				t.Errorf("error missing %q: %v", part, err)
			}
		}
		assertQuiet(t, msg, "live-one")
	}
}

func TestLiveFallbackOrder(t *testing.T) {
	first := &probeCell{}
	first.clear()
	second := &probeCell{value: "second-one"}
	reg := liveRegistry(map[string]*probeCell{"first": first, "second": second})
	doc := "live = [{source=\"first\", var=\"FIRST\", read=\"at_use\"}, {source=\"second\", var=\"SECOND\", read=\"at_use\"}]\n"
	var got liveSingle
	plan, err := Load(t.Context(), &got, Config{
		Path:      writeDoc(t, doc),
		LookupEnv: mapLookup(nil),
		Registry:  reg,
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	line := plan.String()
	if !strings.Contains(line, "live\tsecond\tvar=SECOND\tresolved") {
		t.Errorf("plan = %q", line)
	}
	first.set("first-two")
	val, err := got.Live.Reveal()
	if err != nil {
		t.Fatalf("Reveal: %v", err)
	}
	if val != "first-two" {
		t.Errorf("Reveal = %q, want first-two", val)
	}
	first.clear()
	second.set("second-two")
	val, err = got.Live.Reveal()
	if err != nil {
		t.Fatalf("Reveal: %v", err)
	}
	if val != "second-two" {
		t.Errorf("Reveal = %q, want second-two", val)
	}
}

func TestLiveIgnoresCancel(t *testing.T) {
	cell := &probeCell{value: "live-one"}
	reg := liveRegistry(map[string]*probeCell{"live": cell})
	doc := "[live]\nsource = \"live\"\nvar = \"LIVE_KEY\"\nread = \"at_use\"\n"
	ctx, cancel := context.WithCancel(t.Context())
	var got liveSingle
	if _, err := Load(ctx, &got, Config{
		Path:      writeDoc(t, doc),
		LookupEnv: mapLookup(nil),
		Registry:  reg,
	}); err != nil {
		t.Fatalf("Load: %v", err)
	}
	cancel()
	cell.set("live-two")
	val, err := got.Live.Reveal()
	if err != nil {
		t.Fatalf("Reveal after cancel: %v", err)
	}
	if val != "live-two" {
		t.Errorf("Reveal = %q, want live-two", val)
	}
	if err := ctx.Err(); err == nil {
		t.Fatalf("load context is not canceled")
	}
}

type ctxTag struct{}

type valueCell struct {
	mu        sync.Mutex
	value     string
	seen      []any
	deadlines []bool
}

func (c *valueCell) set(value string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.value = value
}

func (c *valueCell) calls() ([]any, []bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]any(nil), c.seen...), append([]bool(nil), c.deadlines...)
}

type valueSource struct {
	cell *valueCell
}

func (s valueSource) Resolve(ctx context.Context, ref Ref) (string, error) {
	_, hasDeadline := ctx.Deadline()
	s.cell.mu.Lock()
	s.cell.seen = append(s.cell.seen, ctx.Value(ctxTag{}))
	s.cell.deadlines = append(s.cell.deadlines, hasDeadline)
	val := s.cell.value
	s.cell.mu.Unlock()
	return val, nil
}

func TestLiveKeepsValuesWithoutDeadline(t *testing.T) {
	cell := &valueCell{value: "live-one"}
	reg := NewRegistry()
	if err := reg.Register("live", valueSource{cell: cell}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	doc := "[live]\nsource = \"live\"\nvar = \"LIVE_KEY\"\nread = \"at_use\"\n"
	ctx, cancel := context.WithDeadline(
		context.WithValue(t.Context(), ctxTag{}, "kept"),
		time.Now().Add(time.Hour),
	)
	defer cancel()
	var got liveSingle
	if _, err := Load(ctx, &got, Config{
		Path:      writeDoc(t, doc),
		LookupEnv: mapLookup(nil),
		Registry:  reg,
	}); err != nil {
		t.Fatalf("Load: %v", err)
	}
	cancel()
	cell.set("live-two")
	val, err := got.Live.Reveal()
	if err != nil {
		t.Fatalf("Reveal after cancel: %v", err)
	}
	if val != "live-two" {
		t.Errorf("Reveal = %q, want live-two", val)
	}
	seen, deadlines := cell.calls()
	if len(seen) != 2 {
		t.Fatalf("recorded %d Resolve calls, want 2", len(seen))
	}
	for i, want := range []any{"kept", "kept"} {
		if seen[i] != want {
			t.Errorf("call %d saw context value %v, want kept", i, seen[i])
		}
	}
	if !deadlines[0] {
		t.Errorf("load Resolve saw no deadline, want one")
	}
	if deadlines[1] {
		t.Errorf("live Reveal kept the load deadline")
	}
}

func TestLiveReadsConcurrently(t *testing.T) {
	cell := &probeCell{value: "live-one"}
	reg := liveRegistry(map[string]*probeCell{"live": cell})
	doc := "[live]\nsource = \"live\"\nvar = \"LIVE_KEY\"\nread = \"at_use\"\n"
	var got liveSingle
	if _, err := Load(t.Context(), &got, Config{
		Path:      writeDoc(t, doc),
		LookupEnv: mapLookup(nil),
		Registry:  reg,
	}); err != nil {
		t.Fatalf("Load: %v", err)
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			val, err := got.Live.Reveal()
			if err != nil {
				t.Errorf("Reveal: %v", err)
				return
			}
			if val != "live-one" {
				t.Errorf("Reveal = %q, want live-one", val)
			}
		}()
	}
	wg.Wait()
}

type textFileSource struct{}

func (textFileSource) Resolve(ctx context.Context, ref Ref) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	if ref.Path() == "" {
		return "", errors.New("stub file source needs a path")
	}
	raw, err := os.ReadFile(ref.Path())
	if err != nil {
		return "", err
	}
	return strings.TrimSuffix(string(raw), "\n"), nil
}

type mixedPair struct {
	Frozen Secret `toml:"frozen"`
	Live   Secret `toml:"live"`
}

func TestMixedWinnerDecidesMode(t *testing.T) {
	bootWinner := &probeCell{value: "boot-one"}
	liveAlt := &probeCell{value: "alt-one"}
	miss := &probeCell{}
	miss.clear()
	liveWinner := &probeCell{value: "live-one"}
	bootLate := &probeCell{}
	bootLate.clear()
	reg := liveRegistry(map[string]*probeCell{
		"bootWinner": bootWinner,
		"liveAlt":    liveAlt,
		"miss":       miss,
		"liveWinner": liveWinner,
		"bootLate":   bootLate,
	})
	doc := "frozen = [{source=\"bootWinner\", var=\"BOOT\"}, " +
		"{source=\"liveAlt\", var=\"ALT\", read=\"at_use\"}]\n" +
		"live = [{source=\"miss\", var=\"MISS\"}, " +
		"{source=\"liveWinner\", var=\"LIVE\", read=\"at_use\"}, " +
		"{source=\"bootLate\", var=\"LATE\"}]\n"
	var got mixedPair
	if _, err := Load(t.Context(), &got, Config{
		Path:      writeDoc(t, doc),
		LookupEnv: mapLookup(nil),
		Registry:  reg,
	}); err != nil {
		t.Fatalf("Load: %v", err)
	}
	liveAlt.set("alt-two")
	val, err := got.Frozen.Reveal()
	if err != nil {
		t.Fatalf("frozen Reveal: %v", err)
	}
	if val != "boot-one" {
		t.Errorf("frozen Reveal = %q, want boot-one", val)
	}
	bootWinner.clear()
	val, err = got.Frozen.Reveal()
	if err != nil {
		t.Fatalf("frozen Reveal after delete: %v", err)
	}
	if val != "boot-one" {
		t.Errorf("frozen Reveal = %q, want boot-one", val)
	}
	liveWinner.set("live-two")
	val, err = got.Live.Reveal()
	if err != nil {
		t.Fatalf("live Reveal: %v", err)
	}
	if val != "live-two" {
		t.Errorf("live Reveal = %q, want live-two", val)
	}
	liveWinner.clear()
	bootLate.set("boot-late")
	val, err = got.Live.Reveal()
	if err != nil {
		t.Fatalf("live Reveal with boot fallback: %v", err)
	}
	if val != "boot-late" {
		t.Errorf("live Reveal = %q, want boot-late", val)
	}
}
func TestLiveTracksRealFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "token")
	if err := os.WriteFile(path, []byte("file-one\n"), 0o600); err != nil {
		t.Fatalf("write secret file: %v", err)
	}
	reg := NewRegistry()
	if err := reg.Register("stubfile", textFileSource{}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	doc := "[live]\nsource = \"stubfile\"\npath = " + strconv.Quote(path) + "\nread = \"at_use\"\n"
	var got liveSingle
	if _, err := Load(t.Context(), &got, Config{
		Path:      writeDoc(t, doc),
		LookupEnv: mapLookup(nil),
		Registry:  reg,
	}); err != nil {
		t.Fatalf("Load: %v", err)
	}
	if val, err := got.Live.Reveal(); err != nil {
		t.Fatalf("Reveal: %v", err)
	} else if val != "file-one" {
		t.Fatalf("Reveal = %q, want file-one", val)
	}
	if err := os.WriteFile(path, []byte("file-two\n"), 0o600); err != nil {
		t.Fatalf("rewrite secret file: %v", err)
	}
	if val, err := got.Live.Reveal(); err != nil {
		t.Fatalf("Reveal after rewrite: %v", err)
	} else if val != "file-two" {
		t.Errorf("Reveal = %q, want file-two", val)
	}
	if err := os.Remove(path); err != nil {
		t.Fatalf("remove secret file: %v", err)
	}
	if _, err := got.Live.Reveal(); err == nil {
		t.Fatalf("Reveal after delete succeeded, want failure")
	} else {
		if !errors.Is(err, ErrResolve) {
			t.Errorf("err = %v, want ErrResolve", err)
		}
		assertQuiet(t, err.Error(), "file-two")
	}
	if _, err := got.Live.Reveal(); err == nil {
		t.Fatalf("second Reveal after delete succeeded, want failure")
	}
}

type planPair struct {
	Boot Secret `toml:"boot"`
	Live Secret `toml:"live"`
}

func TestPlanStaysStableAcrossRotation(t *testing.T) {
	boot := &probeCell{value: "boot-one"}
	live := &probeCell{value: "live-one"}
	reg := liveRegistry(map[string]*probeCell{"boot": boot, "live": live})
	doc := "[boot]\nsource = \"boot\"\nvar = \"BOOT_KEY\"\n[live]\nsource = \"live\"\nvar = \"LIVE_KEY\"\nread = \"at_use\"\n"
	var got planPair
	plan, err := Load(t.Context(), &got, Config{
		Path:      writeDoc(t, doc),
		LookupEnv: mapLookup(nil),
		Registry:  reg,
	})
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	before := plan.String()
	dump, err := Dump(&got, plan)
	if err != nil {
		t.Fatalf("Dump: %v", err)
	}
	firstDump := string(dump)
	boot.set("boot-two")
	live.set("live-two")
	if val, err := got.Live.Reveal(); err != nil {
		t.Fatalf("Reveal: %v", err)
	} else if val != "live-two" {
		t.Fatalf("Reveal = %q, want live-two", val)
	}
	if after := plan.String(); after != before {
		t.Errorf("plan changed across rotation:\nbefore %q\nafter %q", before, after)
	}
	dump, err = Dump(&got, plan)
	if err != nil {
		t.Fatalf("Dump after rotation: %v", err)
	}
	if string(dump) != firstDump {
		t.Errorf("dump changed across rotation")
	}
	assertQuiet(t, before, "boot-one", "boot-two", "live-one", "live-two")
	assertQuiet(t, firstDump, "boot-one", "boot-two", "live-one", "live-two")
}
