package config

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// stubSource is a Source that resolves to a fixed value. It stands in for a
// built-in while the reference contract is under test.
type stubSource struct {
	value   string
	failure error
}

func (s stubSource) Resolve(ctx context.Context, ref Ref) (string, error) {
	return s.value, s.failure
}

// stubRegistry returns a Registry holding the five built-in source names.
func stubRegistry(t *testing.T) *Registry {
	t.Helper()
	reg := NewRegistry()
	for _, name := range []string{"env", "env_file", "file", "dir", "command"} {
		if err := reg.Register(name, stubSource{}); err != nil {
			t.Fatalf("register %q: %v", name, err)
		}
	}
	return reg
}

type testSecrets struct {
	Key      Secret `toml:"key"`
	Other    Secret `toml:"other"`
	Ordered  Secret `toml:"ordered"`
	Settings Secret `toml:"-"`
}

type testSettings struct {
	Render struct {
		Model string `toml:"model"`
	} `toml:"render"`
	Secrets testSecrets `toml:"secrets"`
}

// decode extracts the Secrets section from a document.
func decode(t *testing.T, doc string) testSettings {
	t.Helper()
	var got testSettings
	if err := Decode([]byte(doc), &got, stubRegistry(t)); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	return got
}

func TestDecodeSourceShapes(t *testing.T) {
	cases := []struct {
		name string
		body string
		want Ref
		loc  string
	}{
		{
			name: "env",
			body: "source = \"env\"\nvar = \"PROVIDER_KEY\"",
			want: Ref{source: "env", variable: "PROVIDER_KEY", read: ReadAtBoot},
			loc:  "var=PROVIDER_KEY",
		},
		{
			name: "env_file",
			body: "source = \"env_file\"\npath = \"/etc/app/env\"\nvar = \"PROVIDER_KEY\"",
			want: Ref{source: "env_file", path: "/etc/app/env", variable: "PROVIDER_KEY", read: ReadAtBoot},
			loc:  "path=/etc/app/env var=PROVIDER_KEY",
		},
		{
			name: "file",
			body: "source = \"file\"\npath = \"/run/secrets/provider_key\"",
			want: Ref{source: "file", path: "/run/secrets/provider_key", read: ReadAtBoot},
			loc:  "path=/run/secrets/provider_key",
		},
		{
			name: "dir",
			body: "source = \"dir\"\npath = \"/run/credentials\"\nname = \"token\"",
			want: Ref{source: "dir", path: "/run/credentials", name: "token", read: ReadAtBoot},
			loc:  "path=/run/credentials name=token",
		},
		{
			name: "command",
			body: "source = \"command\"\ncommand = \"op\"\nargs = [\"read\", \"op://vault/item/field\"]",
			want: Ref{source: "command", command: "op", args: []string{"read", "op://vault/item/field"}, read: ReadAtBoot},
			loc:  "command=op args=read op://vault/item/field",
		},
		{
			name: "read at use",
			body: "source = \"env\"\nvar = \"X\"\nread = \"at_use\"",
			want: Ref{source: "env", variable: "X", read: ReadAtUse},
			loc:  "var=X",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := decode(t, "[secrets.key]\n"+tc.body+"\n")
			refs := got.Secrets.Key.refs
			if len(refs) != 1 {
				t.Fatalf("got %d refs, want 1", len(refs))
			}
			if !reflect.DeepEqual(refs[0], tc.want) {
				t.Errorf("ref = %+v, want %+v", refs[0], tc.want)
			}
			if got := refs[0].locator(); got != tc.loc {
				t.Errorf("Locator = %q, want %q", got, tc.loc)
			}
		})
	}
}

func TestDecodeOrderedList(t *testing.T) {
	t.Run("inline array", func(t *testing.T) {
		got := decode(t, "[secrets]\nkey = [{source=\"env\", var=\"A\"}, {source=\"file\", path=\"/x\"}]\n")
		refs := got.Secrets.Key.refs
		if len(refs) != 2 {
			t.Fatalf("got %d refs, want 2", len(refs))
		}
		if refs[0].Source() != "env" || refs[1].Source() != "file" {
			t.Errorf("order = %q, %q", refs[0].Source(), refs[1].Source())
		}
	})

	t.Run("array of tables", func(t *testing.T) {
		doc := "[[secrets.ordered]]\nsource = \"env\"\nvar = \"A\"\n[[secrets.ordered]]\nsource = \"file\"\npath = \"/x\"\n"
		got := decode(t, doc)
		refs := got.Secrets.Ordered.refs
		if len(refs) != 2 {
			t.Fatalf("got %d refs, want 2", len(refs))
		}
		if refs[0].Source() != "env" || refs[1].Source() != "file" {
			t.Errorf("order = %q, %q", refs[0].Source(), refs[1].Source())
		}
	})
}

func TestDecodeInlineValueRefused(t *testing.T) {
	doc := "[render]\nmodel = \"x\"\n\n[secrets]\nkey = \"sk-live-123\"\n"
	var got testSettings
	err := Decode([]byte(doc), &got, stubRegistry(t))
	if !errors.Is(err, ErrInlineSecret) {
		t.Fatalf("err = %v, want ErrInlineSecret", err)
	}
	if !strings.Contains(err.Error(), "secrets.key") {
		t.Errorf("error does not name the key: %v", err)
	}
	if strings.Contains(err.Error(), "sk-live") {
		t.Errorf("error carries the value: %v", err)
	}
}

func TestDecodeInlineValueRefusedInList(t *testing.T) {
	doc := "[secrets]\nkey = [\"sk-live-123\"]\n"
	var got testSettings
	if err := Decode([]byte(doc), &got, stubRegistry(t)); !errors.Is(err, ErrInlineSecret) {
		t.Fatalf("err = %v, want ErrInlineSecret", err)
	}
}

func TestDecodeUnknownSource(t *testing.T) {
	doc := "[secrets.key]\nsource = \"enviroment\"\nvar = \"X\"\n"
	var got testSettings
	err := Decode([]byte(doc), &got, stubRegistry(t))
	if !errors.Is(err, ErrUnknownSource) {
		t.Fatalf("err = %v, want ErrUnknownSource", err)
	}
	if !strings.Contains(err.Error(), "secrets.key") || !strings.Contains(err.Error(), "enviroment") {
		t.Errorf("error must name the key and the source: %v", err)
	}
}

func TestDecodeUnknownLocatorKeyNamesKeyAndPosition(t *testing.T) {
	doc := "[render]\nmodel = \"x\"\n\n[secrets.key]\nsource = \"env\"\nvr = \"X\"\n"
	var got testSettings
	err := Decode([]byte(doc), &got, stubRegistry(t))
	if !errors.Is(err, ErrInvalidRef) {
		t.Fatalf("err = %v, want ErrInvalidRef", err)
	}
	if !strings.Contains(err.Error(), `"vr"`) {
		t.Errorf("error does not name the key: %v", err)
	}
	if !strings.Contains(err.Error(), "secrets.key") {
		t.Errorf("error does not name the setting: %v", err)
	}
	if !strings.Contains(err.Error(), "line 6") || !strings.Contains(err.Error(), "column 1") {
		t.Errorf("error does not give the position in the file: %v", err)
	}
}

func TestDecodeUnknownKey(t *testing.T) {
	doc := "[render]\nmodel = \"x\"\ntypo = 1\n"
	var got testSettings
	err := Decode([]byte(doc), &got, stubRegistry(t))
	if !errors.Is(err, ErrUnknownKey) {
		t.Fatalf("err = %v, want ErrUnknownKey", err)
	}
	if !strings.Contains(err.Error(), "render.typo") {
		t.Errorf("error does not name the key: %v", err)
	}
}

func TestDecodeBadDestination(t *testing.T) {
	reg := stubRegistry(t)
	if err := Decode([]byte("x = 1\n"), testSettings{}, reg); !errors.Is(err, ErrInvalidDest) {
		t.Errorf("value target err = %v, want ErrInvalidDest", err)
	}
	if err := Decode([]byte("x = 1\n"), nil, reg); !errors.Is(err, ErrInvalidDest) {
		t.Errorf("nil target err = %v, want ErrInvalidDest", err)
	}
}

func TestDecodeMissingSource(t *testing.T) {
	doc := "[secrets.key]\npath = \"/x\"\n"
	var got testSettings
	if err := Decode([]byte(doc), &got, stubRegistry(t)); !errors.Is(err, ErrInvalidRef) {
		t.Fatalf("err = %v, want ErrInvalidRef", err)
	}
}

func TestDecodeUnknownReadMode(t *testing.T) {
	doc := "[secrets.key]\nsource = \"env\"\nvar = \"X\"\nread = \"sometimes\"\n"
	var got testSettings
	if err := Decode([]byte(doc), &got, stubRegistry(t)); !errors.Is(err, ErrInvalidRef) {
		t.Fatalf("err = %v, want ErrInvalidRef", err)
	}
}

func TestDecodeIgnoresUnexportedAndSkippedFields(t *testing.T) {
	got := decode(t, "[secrets.key]\nsource = \"env\"\nvar = \"X\"\n")
	if len(got.Secrets.Settings.refs) != 0 {
		t.Errorf("a toml:\"-\" field was decoded: %+v", got.Secrets.Settings.refs)
	}
}

func TestRegistry(t *testing.T) {
	reg := NewRegistry()
	if err := reg.Register("env", stubSource{}); err != nil {
		t.Fatalf("register: %v", err)
	}
	if _, ok := reg.Lookup("env"); !ok {
		t.Error("Lookup(env) missed")
	}
	if _, ok := reg.Lookup("missing"); ok {
		t.Error("Lookup(missing) hit")
	}

	cases := []struct {
		name string
		call func() error
		want error
	}{
		{"duplicate", func() error { return reg.Register("env", stubSource{}) }, ErrDuplicateSource},
		{"empty name", func() error { return reg.Register("", stubSource{}) }, ErrInvalidSourceName},
		{"space in name", func() error { return reg.Register("env file", stubSource{}) }, ErrInvalidSourceName},
		{"leading digit", func() error { return reg.Register("9lives", stubSource{}) }, ErrInvalidSourceName},
		{"nil source", func() error { return reg.Register("nil", nil) }, ErrNilSource},
		{"nil registry", func() error { return (*Registry)(nil).Register("env", stubSource{}) }, ErrNilRegistry},
	}
	for _, tc := range cases {
		err := tc.call()
		if err == nil {
			t.Errorf("%s accepted", tc.name)
			continue
		}
		if !errors.Is(err, tc.want) {
			t.Errorf("%s err = %v, want %v", tc.name, err, tc.want)
		}
		if errors.Is(err, ErrUnknownSource) {
			t.Errorf("%s error is ErrUnknownSource: %v", tc.name, err)
		}
	}

	if _, ok := (*Registry)(nil).Lookup("env"); ok {
		t.Error("nil Registry Lookup hit")
	}
	reg2 := NewRegistry()
	for _, name := range []string{"file", "env", "command"} {
		if err := reg2.Register(name, stubSource{}); err != nil {
			t.Fatalf("register %q: %v", name, err)
		}
	}
	want := []string{"command", "env", "file"}
	if got := reg2.names(); !reflect.DeepEqual(got, want) {
		t.Errorf("names = %v, want %v", got, want)
	}
}

func TestSecretReveal(t *testing.T) {
	var zero Secret
	if _, err := zero.Reveal(); !errors.Is(err, ErrUnresolved) {
		t.Errorf("zero Reveal err = %v, want ErrUnresolved", err)
	}
	got := decode(t, "[secrets.key]\nsource = \"env\"\nvar = \"X\"\n")
	if _, err := got.Secrets.Key.Reveal(); !errors.Is(err, ErrUnresolved) {
		t.Errorf("undecoded value reveals without a loader: %v", err)
	}
	if got.Secrets.Key.scan.doc != nil || got.Secrets.Key.scan.key != "" {
		t.Errorf("decoded secret keeps the decode context: %+v", got.Secrets.Key.scan)
	}
}

func TestSecretNeverPrintsValue(t *testing.T) {
	const value = "sk-live-1234567890"
	s := Secret{reveal: func() (string, error) { return value, nil }}
	if got, _ := s.Reveal(); got != value {
		t.Fatalf("Reveal = %q, want the value", got)
	}
	encoded, err := json.Marshal(s)
	if err != nil {
		t.Fatalf("MarshalJSON: %v", err)
	}
	var fromJSON string
	if err := json.Unmarshal(encoded, &fromJSON); err != nil {
		t.Fatalf("unmarshal json: %v", err)
	}
	printed := map[string]string{
		"%v":            fmt.Sprintf("%v", s),
		"%s":            fmt.Sprintf("%s", s),
		"%q":            fmt.Sprintf("%q", s),
		"%#v":           fmt.Sprintf("%#v", s),
		"struct field":  fmt.Sprintf("%v", struct{ S Secret }{s}),
		"slice element": fmt.Sprintf("%v", []Secret{s}),
		"json":          fromJSON,
		"slog":          fmt.Sprint(s.LogValue().Any()),
	}
	for name, out := range printed {
		if strings.Contains(out, value) {
			t.Errorf("%s output carries the value: %q", name, out)
		}
		if !strings.Contains(out, redacted) {
			t.Errorf("%s output is not the placeholder: %q", name, out)
		}
	}
	if _, err := s.MarshalText(); err != nil {
		t.Fatalf("MarshalText: %v", err)
	}
}

// mapSecrets, ptrSecrets, sliceSecrets, nestedSettings and embeddedSettings
// carry the field shapes an application can write, so the shape tests decode
// the same document into each of them.
type mapSecrets struct {
	Secrets map[string]Secret `toml:"secrets"`
}

type ptrSecrets struct {
	Key *Secret `toml:"key"`
}

type sliceSecrets struct {
	Key []Secret `toml:"key"`
}

type nestedSettings struct {
	Nested struct {
		Key Secret `toml:"key"`
	} `toml:"nested"`
}

type credBase struct {
	Cred Secret `toml:"cred"`
}

type embeddedSettings struct {
	credBase
}

func TestDecodeRefusesUnknownSourceOnEveryShape(t *testing.T) {
	reg := stubRegistry(t)
	cases := []struct {
		name string
		dst  any
		doc  string
		key  string
	}{
		{"value", &struct {
			Key Secret `toml:"key"`
		}{}, "[key]\nsource = \"envv\"\nvar = \"X\"\n", "key"},
		{"pointer", &ptrSecrets{}, "[key]\nsource = \"envv\"\nvar = \"X\"\n", "key"},
		{"map", &mapSecrets{}, "[secrets.key]\nsource = \"envv\"\nvar = \"X\"\n", "secrets.key"},
		{"array of tables", &sliceSecrets{}, "[[key]]\nsource = \"envv\"\nvar = \"X\"\n", "key"},
		{"inline array", &sliceSecrets{}, "key = [{source = \"envv\", var = \"X\"}]\n", "key"},
		{"nested struct", &nestedSettings{}, "[nested.key]\nsource = \"envv\"\nvar = \"X\"\n", "nested.key"},
		{"embedded struct", &embeddedSettings{}, "[cred]\nsource = \"envv\"\nvar = \"X\"\n", "cred"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := Decode([]byte(tc.doc), tc.dst, reg)
			if !errors.Is(err, ErrUnknownSource) {
				t.Fatalf("err = %v, want ErrUnknownSource", err)
			}
			if !strings.Contains(err.Error(), tc.key) {
				t.Errorf("error does not name the key: %v", err)
			}
		})
	}
}

func TestDecodeRefusesInlineValueOnEveryShape(t *testing.T) {
	reg := stubRegistry(t)
	cases := []struct {
		name string
		dst  any
		doc  string
		key  string
	}{
		{"value", &struct {
			Key Secret `toml:"key"`
		}{}, "key = \"sk-live-123\"\n", "key"},
		{"pointer", &ptrSecrets{}, "key = \"sk-live-123\"\n", "key"},
		{"map", &mapSecrets{}, "[secrets]\nkey = \"sk-live-123\"\n", "secrets.key"},
		{"list element", &sliceSecrets{}, "key = [\"sk-live-123\"]\n", "key"},
		{"scalar on list", &sliceSecrets{}, "key = \"sk-live-123\"\n", "key"},
		{"nested struct", &nestedSettings{}, "[nested]\nkey = \"sk-live-123\"\n", "nested.key"},
		{"embedded struct", &embeddedSettings{}, "cred = \"sk-live-123\"\n", "cred"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := Decode([]byte(tc.doc), tc.dst, reg)
			if !errors.Is(err, ErrInlineSecret) {
				t.Fatalf("err = %v, want ErrInlineSecret", err)
			}
			if !strings.Contains(err.Error(), tc.key) {
				t.Errorf("error does not name the key: %v", err)
			}
			if strings.Contains(err.Error(), "sk-live") {
				t.Errorf("error carries the value: %v", err)
			}
		})
	}
}

func TestDecodeEmbeddedSecretDecodes(t *testing.T) {
	var embedded embeddedSettings
	if err := Decode([]byte("[cred]\nsource = \"env\"\nvar = \"X\"\n"), &embedded, stubRegistry(t)); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	refs := embedded.Cred.refs
	if len(refs) != 1 || refs[0].Source() != "env" {
		t.Errorf("embedded secret decoded to %+v", refs)
	}
}

func TestDecodeRefFailureReportsFilePosition(t *testing.T) {
	reg := stubRegistry(t)
	filler := strings.Repeat("# filler line, the file is not empty\n", 3)
	cases := []struct {
		name string
		line string
	}{
		{"table body", "[secrets.key]\nsource = \"env\"\nvr = \"X\"\n"},
		{"inline table", "[secrets]\nkey = { source = \"env\", vr = \"X\" }\n"},
		{"inline array", "[secrets]\nkey = [{ source = \"env\", var = \"A\" }, { source = \"env\", vr = \"X\" }]\n"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			doc := filler + tc.line
			at := strings.Index(doc, "vr")
			if at < 0 {
				t.Fatalf("vr is not in the document")
			}
			line := 1 + strings.Count(doc[:at], "\n")
			start := strings.LastIndexByte(doc[:at], '\n') + 1
			column := at - start + 1
			var got testSettings
			err := Decode([]byte(doc), &got, reg)
			if !errors.Is(err, ErrInvalidRef) {
				t.Fatalf("err = %v, want ErrInvalidRef", err)
			}
			want := fmt.Sprintf("secrets.key: unknown key %q at line %d, column %d", "vr", line, column)
			if !strings.Contains(err.Error(), want) {
				t.Errorf("err = %v, want it to contain %q", err, want)
			}
		})
	}
}

// TestDecodeLocatorTypoOnEveryShape pins the typo contract on the shapes
// the strict decoder creates during the decode. A fresh map value, a
// fresh array of tables element, a fresh inline array element, and a
// preset slice element each fail with the dotted key and the position
// in the file.
func TestDecodeLocatorTypoOnEveryShape(t *testing.T) {
	reg := stubRegistry(t)
	filler := strings.Repeat("# filler line, the file is not empty\n", 3)
	cases := []struct {
		name string
		dst  func() any
		doc  string
		key  string
	}{
		{"fresh map value", func() any { return &mapSecrets{} }, "[secrets.key]\nsource = \"env\"\nvr = \"X\"\n", "secrets.key"},
		{"fresh array of tables", func() any { return &sliceSecrets{} }, "[[key]]\nsource = \"env\"\nvr = \"X\"\n", "key"},
		{"fresh inline array", func() any { return &sliceSecrets{} }, "key = [{ source = \"env\", var = \"A\" }, { source = \"env\", vr = \"X\" }]\n", "key"},
		{"preset slice element", func() any {
			dst := &sliceSecrets{}
			dst.Key = []Secret{{}}
			return dst
		}, "[[key]]\nsource = \"env\"\nvr = \"X\"\n", "key"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			doc := filler + tc.doc
			at := strings.Index(doc, "vr")
			if at < 0 {
				t.Fatalf("vr is not in the document")
			}
			line := 1 + strings.Count(doc[:at], "\n")
			start := strings.LastIndexByte(doc[:at], '\n') + 1
			column := at - start + 1
			err := Decode([]byte(doc), tc.dst(), reg)
			if !errors.Is(err, ErrInvalidRef) {
				t.Fatalf("err = %v, want ErrInvalidRef", err)
			}
			want := fmt.Sprintf("%s: unknown key %q at line %d, column %d", tc.key, "vr", line, column)
			if !strings.Contains(err.Error(), want) {
				t.Errorf("err = %v, want it to contain %q", err, want)
			}
			if strings.Contains(err.Error(), "incomplete") || strings.Contains(err.Error(), "fragment") {
				t.Errorf("err reports the fragment, not the file: %v", err)
			}
		})
	}
}

func TestDecodeTwiceDoesNotAccumulate(t *testing.T) {
	doc := "[[secrets.ordered]]\nsource = \"env\"\nvar = \"A\"\n"
	var got testSettings
	reg := stubRegistry(t)
	if err := Decode([]byte(doc), &got, reg); err != nil {
		t.Fatalf("first Decode: %v", err)
	}
	if err := Decode([]byte(doc), &got, reg); err != nil {
		t.Fatalf("second Decode: %v", err)
	}
	if len(got.Secrets.Ordered.refs) != 1 {
		t.Errorf("got %d refs, want 1", len(got.Secrets.Ordered.refs))
	}
}

// listBase carries an ordered secret list as an embedded struct field.
type listBase struct {
	Cred []Secret `toml:"cred"`
}

// listShapes holds an ordered secret list behind every shape the type walk
// reaches, so one document fills them all.
type listShapes struct {
	listBase
	Key    []Secret  `toml:"key"`
	Ptr    *[]Secret `toml:"ptr"`
	Nested struct {
		Keys []Secret `toml:"keys"`
	} `toml:"nested"`
	Render struct {
		Model string `toml:"model"`
	} `toml:"render"`
	One Secret `toml:"one"`
}

// TestDecodeInlineArrayOnEveryListShape pins the ordered list written as an
// inline array on each shape the type walk reaches. go-toml hands a slice
// element only the opening brace of its inline table, so the package reads
// the array itself and injects the references afterwards.
func TestDecodeInlineArrayOnEveryListShape(t *testing.T) {
	reg := stubRegistry(t)
	two := `[{source = "env", var = "A"}, {source = "file", path = "/x"}]`
	cases := []struct {
		name string
		doc  string
		list func(listShapes) []Secret
	}{
		{"top level", "key = " + two + "\n", func(g listShapes) []Secret { return g.Key }},
		{"pointer", "ptr = " + two + "\n", func(g listShapes) []Secret {
			if g.Ptr == nil {
				return nil
			}
			return *g.Ptr
		}},
		{"nested struct", "[nested]\nkeys = " + two + "\n", func(g listShapes) []Secret { return g.Nested.Keys }},
		{"embedded struct", "cred = " + two + "\n", func(g listShapes) []Secret { return g.Cred }},
		{"multi-line", "key = [\n  {source = \"env\", var = \"A\"},\n  {source = \"file\", path = \"/x\"},\n]\n", func(g listShapes) []Secret { return g.Key }},
		{"two arrays in one document", "ptr = " + two + "\nkey = " + two + "\n", func(g listShapes) []Secret { return g.Key }},
	}
	want := []Ref{
		{source: "env", variable: "A", read: ReadAtBoot},
		{source: "file", path: "/x", read: ReadAtBoot},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got listShapes
			if err := Decode([]byte(tc.doc), &got, reg); err != nil {
				t.Fatalf("Decode: %v", err)
			}
			list := tc.list(got)
			if len(list) != len(want) {
				t.Fatalf("got %d secrets, want %d", len(list), len(want))
			}
			for i, ref := range want {
				got := list[i].refs
				if len(got) != 1 || !reflect.DeepEqual(got[0], ref) {
					t.Errorf("element %d = %+v, want one reference %+v", i, got, ref)
				}
			}
			if tc.name == "two arrays in one document" {
				if got.Ptr == nil || len(*got.Ptr) != len(want) {
					t.Fatalf("the second list holds %v, want %d secrets", got.Ptr, len(want))
				}
			}
		})
	}
}

// TestDecodeInlineArrayKeepsLaterPositions pins that the decode copy keeps
// every byte count, so a failure after an inline array still reports the
// position in the file the operator edits.
func TestDecodeInlineArrayKeepsLaterPositions(t *testing.T) {
	cases := []struct {
		name string
		doc  string
		want string
	}{
		{
			"single line array",
			"key = [{source = \"env\", var = \"A\"}]\n[render]\nmodel = \"x\"\ntypo = 1\n",
			`"render.typo" at line 4, column 1`,
		},
		{
			"multi-line array",
			"key = [\n  {source = \"env\", var = \"A\"},\n  {source = \"file\", path = \"/x\"},\n]\n[render]\nmodel = \"x\"\ntypo = 1\n",
			`"render.typo" at line 7, column 1`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got listShapes
			err := Decode([]byte(tc.doc), &got, stubRegistry(t))
			if !errors.Is(err, ErrUnknownKey) {
				t.Fatalf("err = %v, want ErrUnknownKey", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want the key at %s", err, tc.want)
			}
		})
	}
}

// TestDecodePlainSecretInlineArrayStaysOneSecret pins that an inline array
// on a single secret setting still decodes as one secret holding the
// ordered references.
func TestDecodePlainSecretInlineArrayStaysOneSecret(t *testing.T) {
	doc := "one = [{source = \"env\", var = \"A\"}, {source = \"file\", path = \"/x\"}]\n"
	var got listShapes
	if err := Decode([]byte(doc), &got, stubRegistry(t)); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	refs := got.One.refs
	if len(refs) != 2 || refs[0].Source() != "env" || refs[1].Source() != "file" {
		t.Errorf("one = %+v, want the two ordered references", refs)
	}
}

// TestDecodeEmptyInlineArrayRefused pins that an inline array with no
// reference is refused by name, the same way an empty table body is.
func TestDecodeEmptyInlineArrayRefused(t *testing.T) {
	var got listShapes
	err := Decode([]byte("key = []\n"), &got, stubRegistry(t))
	if !errors.Is(err, ErrInvalidRef) {
		t.Fatalf("err = %v, want ErrInvalidRef", err)
	}
	if !strings.Contains(err.Error(), "key") {
		t.Errorf("error does not name the key: %v", err)
	}
}

// fixedSecrets holds an ordered secret list as a fixed size array.
type fixedSecrets struct {
	Key [2]Secret `toml:"key"`
}

// rowSecrets holds an ordered secret list inside a row of an array of tables.
type rowSecrets struct {
	Rows []struct {
		Keys []Secret `toml:"keys"`
	} `toml:"rows"`
}

// TestDecodeFixedSizeSecretArray pins the fixed size array, which takes the
// references the document writes or refuses the document by name.
func TestDecodeFixedSizeSecretArray(t *testing.T) {
	reg := stubRegistry(t)
	t.Run("exact count", func(t *testing.T) {
		doc := "key = [{source = \"env\", var = \"A\"}, {source = \"file\", path = \"/x\"}]\n"
		var got fixedSecrets
		if err := Decode([]byte(doc), &got, reg); err != nil {
			t.Fatalf("Decode: %v", err)
		}
		if got.Key[0].refs[0].Source() != "env" || got.Key[1].refs[0].Source() != "file" {
			t.Errorf("array = %+v, want the two references in order", got.Key)
		}
	})
	t.Run("wrong count", func(t *testing.T) {
		doc := "key = [{source = \"env\", var = \"A\"}]\n"
		var got fixedSecrets
		err := Decode([]byte(doc), &got, reg)
		if !errors.Is(err, ErrInvalidRef) {
			t.Fatalf("err = %v, want ErrInvalidRef", err)
		}
		if !strings.Contains(err.Error(), "key") {
			t.Errorf("error does not name the key: %v", err)
		}
		for i, element := range got.Key {
			if len(element.refs) != 0 {
				t.Errorf("element %d holds %+v, want the document refused", i, element.refs)
			}
		}
	})
	t.Run("unknown source refused", func(t *testing.T) {
		doc := "key = [{source = \"envv\", var = \"A\"}, {source = \"env\", var = \"B\"}]\n"
		var got fixedSecrets
		err := Decode([]byte(doc), &got, reg)
		if !errors.Is(err, ErrUnknownSource) {
			t.Fatalf("err = %v, want ErrUnknownSource", err)
		}
		if !strings.Contains(err.Error(), "key") {
			t.Errorf("error does not name the key: %v", err)
		}
	})
	t.Run("literal element refused", func(t *testing.T) {
		doc := "key = [\"sk-live-123\", \"sk-live-456\"]\n"
		var got fixedSecrets
		err := Decode([]byte(doc), &got, reg)
		if !errors.Is(err, ErrInlineSecret) {
			t.Fatalf("err = %v, want ErrInlineSecret", err)
		}
		if strings.Contains(err.Error(), "sk-live") {
			t.Errorf("error carries the value: %v", err)
		}
	})
}

// TestDecodeNestedListInEachRow pins the ordered list written inside an
// element of an array of tables. Each row carries its own list, and the
// element index keeps them apart.
func TestDecodeNestedListInEachRow(t *testing.T) {
	doc := "[[rows]]\nkeys = [{source = \"env\", var = \"A\"}]\n" +
		"[[rows]]\nkeys = [{source = \"file\", path = \"/x\"}, {source = \"env\", var = \"B\"}]\n"
	var got rowSecrets
	if err := Decode([]byte(doc), &got, stubRegistry(t)); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(got.Rows) != 2 {
		t.Fatalf("got %d rows, want 2", len(got.Rows))
	}
	want := [][]string{{"env"}, {"file", "env"}}
	for i, row := range got.Rows {
		if len(row.Keys) != len(want[i]) {
			t.Fatalf("row %d holds %d references, want %d", i, len(row.Keys), len(want[i]))
		}
		for j, source := range want[i] {
			if got := row.Keys[j].refs; len(got) != 1 || got[0].Source() != source {
				t.Errorf("row %d reference %d = %+v, want source %q", i, j, got, source)
			}
		}
	}
}

// TestDecodeKeyPositionIsItsOwn pins that a locator key reports its own
// position, even when the same spelling appears earlier in the file, in a
// comment or inside a longer key.
func TestDecodeKeyPositionIsItsOwn(t *testing.T) {
	reg := stubRegistry(t)
	cases := []struct {
		name string
		doc  string
		want string
	}{
		{"comment names the key", "# a comment that names vr\n[key]\nsource = \"env\"\nvr = 1\n", `unknown key "vr" at line 4, column 1`},
		{"shorter key after a longer one", "[key]\nsource = \"env\"\nvar = \"A\"\nva = 1\n", `unknown key "va" at line 4, column 1`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var got struct {
				Key Secret `toml:"key"`
			}
			err := Decode([]byte(tc.doc), &got, reg)
			if !errors.Is(err, ErrInvalidRef) {
				t.Fatalf("err = %v, want ErrInvalidRef", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("err = %v, want %s", err, tc.want)
			}
		})
	}
}

// caseList holds a list setting whose document key may use another case.
type caseList struct {
	Keys []Secret `toml:"key"`
}

// untaggedList holds a list setting whose document key folds the field name,
// which is how the decoder matches an untagged field.
type untaggedList struct {
	Keys []Secret
}

// rowList holds a list setting inside an element of an array of tables.
type rowList struct {
	Rows []struct {
		Keys []Secret `toml:"keys"`
	} `toml:"rows"`
}

// nestedRowList holds a list setting written under its own header inside an
// element of an array of tables. The header names a map, so its keys name the
// settings below it.
type nestedRowList struct {
	Rows []struct {
		Secrets map[string][]Secret `toml:"secrets"`
	} `toml:"rows"`
}

const twoRefs = `[{source = "env", var = "A"}, {source = "file", path = "/x"}]`

// TestDecodeListKeyCase pins that a list setting keeps its references when the
// document spells the key in another case. Both walks fold case, so the record
// the stash writes must name the setting the way the walk does.
func TestDecodeListKeyCase(t *testing.T) {
	reg := stubRegistry(t)
	cases := []struct {
		name string
		doc  string
		new  func() any
		list func(any) []Secret
	}{
		{"tagged field, other case", "KEY = " + twoRefs + "\n",
			func() any { return &caseList{} },
			func(dst any) []Secret { return dst.(*caseList).Keys }},
		{"tagged field, folded case", "key = " + twoRefs + "\n",
			func() any { return &caseList{} },
			func(dst any) []Secret { return dst.(*caseList).Keys }},
		{"untagged field, folded case", "keys = " + twoRefs + "\n",
			func() any { return &untaggedList{} },
			func(dst any) []Secret { return dst.(*untaggedList).Keys }},
		{"untagged field, field case", "Keys = " + twoRefs + "\n",
			func() any { return &untaggedList{} },
			func(dst any) []Secret { return dst.(*untaggedList).Keys }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dst := tc.new()
			if err := Decode([]byte(tc.doc), dst, reg); err != nil {
				t.Fatalf("Decode: %v", err)
			}
			list := tc.list(dst)
			if len(list) != 2 {
				t.Fatalf("got %d references, want 2", len(list))
			}
			if list[0].refs[0].Source() != "env" || list[1].refs[0].Source() != "file" {
				t.Errorf("list = %+v, want the two references in order", list)
			}
		})
	}
}

// TestDecodeListUnderItsOwnHeaderInARow pins a list written with its own
// header inside an element of an array of tables, and the position of a
// locator typo inside it.
func TestDecodeListUnderItsOwnHeaderInARow(t *testing.T) {
	reg := stubRegistry(t)
	t.Run("array of tables for the list", func(t *testing.T) {
		doc := "[[rows]]\n[[rows.keys]]\nsource = \"env\"\nvar = \"A\"\n" +
			"[[rows.keys]]\nsource = \"file\"\npath = \"/x\"\n"
		var got rowList
		if err := Decode([]byte(doc), &got, reg); err != nil {
			t.Fatalf("Decode: %v", err)
		}
		if len(got.Rows) != 1 || len(got.Rows[0].Keys) != 2 {
			t.Fatalf("rows = %+v, want one row holding two references", got.Rows)
		}
	})
	t.Run("table header for the list", func(t *testing.T) {
		doc := "[[rows]]\n[rows.secrets]\nk = " + twoRefs + "\n"
		var got nestedRowList
		if err := Decode([]byte(doc), &got, reg); err != nil {
			t.Fatalf("Decode: %v", err)
		}
		if len(got.Rows) != 1 || len(got.Rows[0].Secrets["k"]) != 2 {
			t.Fatalf("rows = %+v, want one row holding two references", got.Rows)
		}
	})
	t.Run("unknown source names the key without an index", func(t *testing.T) {
		doc := "[[rows]]\n[[rows.keys]]\nsource = \"envv\"\nvar = \"A\"\n"
		var got rowList
		err := Decode([]byte(doc), &got, reg)
		if !errors.Is(err, ErrUnknownSource) {
			t.Fatalf("err = %v, want ErrUnknownSource", err)
		}
		if !strings.Contains(err.Error(), "rows.keys") {
			t.Errorf("error does not name the setting: %v", err)
		}
		if strings.Contains(err.Error(), "rows.0.keys") {
			t.Errorf("error carries an element index: %v", err)
		}
	})
	t.Run("locator typo names the key at its line", func(t *testing.T) {
		filler := strings.Repeat("# filler line, the file is not empty\n", 3)
		doc := filler + "[[rows]]\n[[rows.keys]]\nsource = \"env\"\nvr = \"A\"\n"
		at := strings.LastIndex(doc, "vr")
		line := 1 + strings.Count(doc[:at], "\n")
		column := at - strings.LastIndex(doc[:at], "\n")
		var got rowList
		err := Decode([]byte(doc), &got, reg)
		if !errors.Is(err, ErrInvalidRef) {
			t.Fatalf("err = %v, want ErrInvalidRef", err)
		}
		want := fmt.Sprintf("rows.keys: unknown key %q at line %d, column %d", "vr", line, column)
		if !strings.Contains(err.Error(), want) {
			t.Errorf("err = %v, want it to contain %q", err, want)
		}
	})
}

// TestDecodeSingleTableOnListRefused pins that one table written on a list
// setting is refused by key, rather than misread as one element.
func TestDecodeSingleTableOnListRefused(t *testing.T) {
	reg := stubRegistry(t)
	docs := []string{
		"[key]\nsource = \"env\"\nvar = \"A\"\n",
		"key = {source = \"env\", var = \"A\"}\n",
	}
	for _, doc := range docs {
		var got caseList
		err := Decode([]byte(doc), &got, reg)
		if !errors.Is(err, ErrInvalidRef) {
			t.Fatalf("err = %v, want ErrInvalidRef", err)
		}
		if !strings.Contains(err.Error(), "key") {
			t.Errorf("error does not name the key: %v", err)
		}
	}
}

// TestDecodePositionIgnoresRepeatedText pins that a reference failure reports
// the setting's own text, not the first place the same text appears.
func TestDecodePositionIgnoresRepeatedText(t *testing.T) {
	doc := "# key = { source = \"env\", var = 1 }\n[secrets]\nkey = { source = \"env\", var = 1 }\n"
	// The parser points at the value it cannot hold, so the expected
	// position is the value rather than the key.
	at := strings.LastIndex(doc, "var = 1") + len("var = ")
	line := 1 + strings.Count(doc[:at], "\n")
	column := at - strings.LastIndex(doc[:at], "\n")
	var got testSettings
	err := Decode([]byte(doc), &got, stubRegistry(t))
	if !errors.Is(err, ErrInvalidRef) {
		t.Fatalf("err = %v, want ErrInvalidRef", err)
	}
	want := fmt.Sprintf("at line %d, column %d", line, column)
	if !strings.Contains(err.Error(), want) {
		t.Errorf("err = %v, want the fault %s", err, want)
	}
}

// nestedOuterRows holds a list inside an array table that itself sits
// inside another array table. Each outer row has its own nested list.
type nestedOuterRows struct {
	Outer []struct {
		Inner []struct {
			Keys []Secret `toml:"keys"`
		} `toml:"inner"`
	} `toml:"outer"`
}

// TestDecodeNestedListInTwoOuterRows pins two outer rows that each hold a
// nested list. A new outer element restarts the nested list index, so the
// second row keeps the references the document wrote.
func TestDecodeNestedListInTwoOuterRows(t *testing.T) {
	doc := "[[outer]]\n[[outer.inner]]\nkeys = " + twoRefs + "\n" +
		"[[outer]]\n[[outer.inner]]\nkeys = [{source = \"env\", var = \"B\"}]\n"
	var got nestedOuterRows
	if err := Decode([]byte(doc), &got, stubRegistry(t)); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(got.Outer) != 2 {
		t.Fatalf("got %d outer rows, want 2", len(got.Outer))
	}
	if len(got.Outer[0].Inner) != 1 || len(got.Outer[0].Inner[0].Keys) != 2 {
		t.Fatalf("row 0 = %+v, want two references", got.Outer[0])
	}
	if len(got.Outer[1].Inner) != 1 || len(got.Outer[1].Inner[0].Keys) != 1 {
		t.Fatalf("row 1 = %+v, want one reference", got.Outer[1])
	}
	ref := got.Outer[1].Inner[0].Keys[0].refs
	if len(ref) != 1 || ref[0].Source() != "env" || ref[0].Var() != "B" {
		t.Errorf("row 1 reference = %+v, want env B", ref)
	}
}

// TestDecodeTypedFaultOnFreshSliceNamesKey pins a locator of the wrong type
// on a fresh array of tables. The message names the field key and the file
// line, not a fragment line, and it carries no element index.
func TestDecodeTypedFaultOnFreshSliceNamesKey(t *testing.T) {
	filler := strings.Repeat("# filler line, the file is not empty\n", 3)
	doc := filler + "[[key]]\nsource = \"env\"\nvar = 1\n"
	at := strings.Index(doc, "var = 1")
	line := 1 + strings.Count(doc[:at], "\n")
	var got sliceSecrets
	err := Decode([]byte(doc), &got, stubRegistry(t))
	if !errors.Is(err, ErrInvalidRef) {
		t.Fatalf("err = %v, want ErrInvalidRef", err)
	}
	if !strings.Contains(err.Error(), "key: ") {
		t.Errorf("error does not name the key: %v", err)
	}
	if strings.Contains(err.Error(), "key.0") {
		t.Errorf("error carries an element index: %v", err)
	}
	want := fmt.Sprintf("at line %d,", line)
	if !strings.Contains(err.Error(), want) {
		t.Errorf("err = %v, want the fault %s", err, want)
	}
}

// TestDecodeUnknownReadModeOnFreshMapNamesKey pins an unknown read mode on
// a fresh map value. The message names the map entry key.
func TestDecodeUnknownReadModeOnFreshMapNamesKey(t *testing.T) {
	doc := "[secrets]\nk = {source = \"env\", read = \"sometimes\"}\n"
	var got mapSecrets
	err := Decode([]byte(doc), &got, stubRegistry(t))
	if !errors.Is(err, ErrInvalidRef) {
		t.Fatalf("err = %v, want ErrInvalidRef", err)
	}
	if !strings.Contains(err.Error(), "secrets.k") {
		t.Errorf("error does not name the map entry: %v", err)
	}
	if !strings.Contains(err.Error(), `unknown read mode "sometimes"`) {
		t.Errorf("error does not name the read mode: %v", err)
	}
}

// TestDecodeSecretArrayOnFreshMapNamesKey pins a locator typo on a Secret
// written as an inline array inside a fresh map. The message names the map
// entry and the file line.
func TestDecodeSecretArrayOnFreshMapNamesKey(t *testing.T) {
	filler := strings.Repeat("# filler line, the file is not empty\n", 3)
	doc := filler + "[secrets]\nk = [{source = \"env\", vr = \"A\"}]\n"
	at := strings.Index(doc, "vr")
	if at < 0 {
		t.Fatalf("vr is not in the document")
	}
	line := 1 + strings.Count(doc[:at], "\n")
	var got mapSecrets
	err := Decode([]byte(doc), &got, stubRegistry(t))
	if !errors.Is(err, ErrInvalidRef) {
		t.Fatalf("err = %v, want ErrInvalidRef", err)
	}
	if !strings.Contains(err.Error(), "secrets.k") {
		t.Errorf("error does not name the map entry: %v", err)
	}
	want := fmt.Sprintf("at line %d,", line)
	if !strings.Contains(err.Error(), want) {
		t.Errorf("err = %v, want the key %s", err, want)
	}
}

// TestDecodeInlineRowsNestedList pins an inline array of structs that holds
// a nested secret list. The list decodes in document order.
func TestDecodeInlineRowsNestedList(t *testing.T) {
	doc := "rows = [{name = \"a\", keys = [{source = \"env\", var = \"A\"}, {source = \"file\", path = \"/x\"}]}]\n"
	var got struct {
		Rows []struct {
			Name string   `toml:"name"`
			Keys []Secret `toml:"keys"`
		} `toml:"rows"`
	}
	if err := Decode([]byte(doc), &got, stubRegistry(t)); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(got.Rows) != 1 || len(got.Rows[0].Keys) != 2 {
		t.Fatalf("got %+v, want two references", got.Rows)
	}
	first := got.Rows[0].Keys[0].refs
	second := got.Rows[0].Keys[1].refs
	if len(first) != 1 || first[0].Source() != "env" || len(second) != 1 || second[0].Source() != "file" {
		t.Errorf("keys = %+v, want env then file", got.Rows[0].Keys)
	}
}

// TestDecodeInlineRowsNestedSecretTypo pins a locator typo on a nested
// Secret inside an inline array of structs. The message names rows.one and
// carries no element index.
func TestDecodeInlineRowsNestedSecretTypo(t *testing.T) {
	filler := strings.Repeat("# filler line, the file is not empty\n", 3)
	doc := filler + "rows = [{name = \"a\", one = {source = \"env\", vr = \"A\"}}]\n"
	at := strings.Index(doc, "vr")
	if at < 0 {
		t.Fatalf("vr is not in the document")
	}
	line := 1 + strings.Count(doc[:at], "\n")
	start := strings.LastIndexByte(doc[:at], '\n') + 1
	column := at - start + 1
	var got struct {
		Rows []struct {
			Name string `toml:"name"`
			One  Secret `toml:"one"`
		} `toml:"rows"`
	}
	err := Decode([]byte(doc), &got, stubRegistry(t))
	if !errors.Is(err, ErrInvalidRef) {
		t.Fatalf("err = %v, want ErrInvalidRef", err)
	}
	want := fmt.Sprintf("rows.one: unknown key %q at line %d, column %d", "vr", line, column)
	if !strings.Contains(err.Error(), want) {
		t.Errorf("err = %v, want it to contain %q", err, want)
	}
	if strings.Contains(err.Error(), "rows.0") {
		t.Errorf("error carries an element index: %v", err)
	}
}

// TestDecodeSliceOfMapsLocatorTypo pins a locator typo on a Secret sitting
// behind a map entry in an inline array of maps. The message names rows.k
// and the file line, and carries no element index.
func TestDecodeSliceOfMapsLocatorTypo(t *testing.T) {
	filler := strings.Repeat("# filler line, the file is not empty\n", 3)
	doc := filler + "rows = [{k = {source = \"env\", vr = \"A\"}}]\n"
	at := strings.Index(doc, "vr")
	if at < 0 {
		t.Fatalf("vr is not in the document")
	}
	line := 1 + strings.Count(doc[:at], "\n")
	start := strings.LastIndexByte(doc[:at], '\n') + 1
	column := at - start + 1
	var got struct {
		Rows []map[string]Secret `toml:"rows"`
	}
	err := Decode([]byte(doc), &got, stubRegistry(t))
	if !errors.Is(err, ErrInvalidRef) {
		t.Fatalf("err = %v, want ErrInvalidRef", err)
	}
	want := fmt.Sprintf("rows.k: unknown key %q at line %d, column %d", "vr", line, column)
	if !strings.Contains(err.Error(), want) {
		t.Errorf("err = %v, want it to contain %q", err, want)
	}
	if strings.Contains(err.Error(), "rows.0") {
		t.Errorf("error carries an element index: %v", err)
	}
}

// TestDecodeSliceOfMapsNestedList pins a nested secret list written as a
// map entry inside an inline array of maps. The list decodes in document
// order.
func TestDecodeSliceOfMapsNestedList(t *testing.T) {
	doc := "rows = [{k = [{source = \"env\", var = \"A\"}, {source = \"file\", path = \"/x\"}]}]\n"
	var got struct {
		Rows []map[string][]Secret `toml:"rows"`
	}
	if err := Decode([]byte(doc), &got, stubRegistry(t)); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(got.Rows) != 1 {
		t.Fatalf("got %d rows, want 1", len(got.Rows))
	}
	keys := got.Rows[0]["k"]
	if len(keys) != 2 {
		t.Fatalf("got %+v, want two references", keys)
	}
	first := keys[0].refs
	second := keys[1].refs
	if len(first) != 1 || first[0].Source() != "env" || len(second) != 1 || second[0].Source() != "file" {
		t.Errorf("k = %+v, want env then file", keys)
	}
}

// TestDecodeNestedArrayLocatorTypo pins a locator typo on a Secret sitting
// inside an array of arrays of structs. The message names rows.one and the
// file line, and carries no element index.
func TestDecodeNestedArrayLocatorTypo(t *testing.T) {
	filler := strings.Repeat("# filler line, the file is not empty\n", 3)
	doc := filler + "rows = [[{one = {source = \"env\", vr = \"A\"}}]]\n"
	at := strings.Index(doc, "vr")
	if at < 0 {
		t.Fatalf("vr is not in the document")
	}
	line := 1 + strings.Count(doc[:at], "\n")
	start := strings.LastIndexByte(doc[:at], '\n') + 1
	column := at - start + 1
	var got struct {
		Rows [][]struct {
			One Secret `toml:"one"`
		} `toml:"rows"`
	}
	err := Decode([]byte(doc), &got, stubRegistry(t))
	if !errors.Is(err, ErrInvalidRef) {
		t.Fatalf("err = %v, want ErrInvalidRef", err)
	}
	want := fmt.Sprintf("rows.one: unknown key %q at line %d, column %d", "vr", line, column)
	if !strings.Contains(err.Error(), want) {
		t.Errorf("err = %v, want it to contain %q", err, want)
	}
	if strings.Contains(err.Error(), "rows.0") {
		t.Errorf("error carries an element index: %v", err)
	}
}

// TestDecodeNestedArrayNestedList pins a nested secret list written inside
// an array of arrays of structs. The list decodes in document order.
func TestDecodeNestedArrayNestedList(t *testing.T) {
	doc := "rows = [[{keys = [{source = \"env\", var = \"A\"}, {source = \"file\", path = \"/x\"}]}]]\n"
	var got struct {
		Rows [][]struct {
			Keys []Secret `toml:"keys"`
		} `toml:"rows"`
	}
	if err := Decode([]byte(doc), &got, stubRegistry(t)); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(got.Rows) != 1 || len(got.Rows[0]) != 1 {
		t.Fatalf("got %+v, want one inner row", got.Rows)
	}
	keys := got.Rows[0][0].Keys
	if len(keys) != 2 {
		t.Fatalf("got %+v, want two references", keys)
	}
	first := keys[0].refs
	second := keys[1].refs
	if len(first) != 1 || first[0].Source() != "env" || len(second) != 1 || second[0].Source() != "file" {
		t.Errorf("keys = %+v, want env then file", keys)
	}
}

// TestDecodeNestedSecretListLocatorTypo pins a locator typo on a nested
// secret list written as an array element. The message names rows and the
// file line, and carries no element index.
func TestDecodeNestedSecretListLocatorTypo(t *testing.T) {
	filler := strings.Repeat("# filler line, the file is not empty\n", 3)
	doc := filler + "rows = [[{source = \"env\", vr = \"A\"}]]\n"
	at := strings.Index(doc, "vr")
	if at < 0 {
		t.Fatalf("vr is not in the document")
	}
	line := 1 + strings.Count(doc[:at], "\n")
	start := strings.LastIndexByte(doc[:at], '\n') + 1
	column := at - start + 1
	var got struct {
		Rows [][]Secret `toml:"rows"`
	}
	err := Decode([]byte(doc), &got, stubRegistry(t))
	if !errors.Is(err, ErrInvalidRef) {
		t.Fatalf("err = %v, want ErrInvalidRef", err)
	}
	want := fmt.Sprintf("rows: unknown key %q at line %d, column %d", "vr", line, column)
	if !strings.Contains(err.Error(), want) {
		t.Errorf("err = %v, want it to contain %q", err, want)
	}
	if strings.Contains(err.Error(), "rows.0") {
		t.Errorf("error carries an element index: %v", err)
	}
}

// TestDecodeNestedSecretList pins a nested secret list written as an array
// element. Each inner list decodes in document order.
func TestDecodeNestedSecretList(t *testing.T) {
	doc := "rows = [[{source = \"env\", var = \"A\"}, {source = \"file\", path = \"/x\"}]]\n"
	var got struct {
		Rows [][]Secret `toml:"rows"`
	}
	if err := Decode([]byte(doc), &got, stubRegistry(t)); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(got.Rows) != 1 {
		t.Fatalf("got %d outer rows, want 1", len(got.Rows))
	}
	keys := got.Rows[0]
	if len(keys) != 2 {
		t.Fatalf("got %+v, want two references", keys)
	}
	first := keys[0].refs
	second := keys[1].refs
	if len(first) != 1 || first[0].Source() != "env" || len(second) != 1 || second[0].Source() != "file" {
		t.Errorf("rows = %+v, want env then file", keys)
	}
}

// TestDecodeNestedSecretArrayLocatorTypo pins a locator typo on a nested
// fixed size secret list. The message names rows, not rows.0.0, and
// carries no element index.
func TestDecodeNestedSecretArrayLocatorTypo(t *testing.T) {
	filler := strings.Repeat("# filler line, the file is not empty\n", 3)
	doc := filler + "rows = [[{source = \"env\", vr = \"A\"}, {source = \"file\", path = \"/x\"}], [{source = \"env\", var = \"B\"}, {source = \"file\", path = \"/y\"}]]\n"
	at := strings.Index(doc, "vr")
	if at < 0 {
		t.Fatalf("vr is not in the document")
	}
	line := 1 + strings.Count(doc[:at], "\n")
	start := strings.LastIndexByte(doc[:at], '\n') + 1
	column := at - start + 1
	var got struct {
		Rows [2][2]Secret `toml:"rows"`
	}
	err := Decode([]byte(doc), &got, stubRegistry(t))
	if !errors.Is(err, ErrInvalidRef) {
		t.Fatalf("err = %v, want ErrInvalidRef", err)
	}
	want := fmt.Sprintf("rows: unknown key %q at line %d, column %d", "vr", line, column)
	if !strings.Contains(err.Error(), want) {
		t.Errorf("err = %v, want it to contain %q", err, want)
	}
	if strings.Contains(err.Error(), "rows.0") {
		t.Errorf("error carries an element index: %v", err)
	}
}

// TestDecodeNestedSecretListCommentBracket pins a nested secret list whose
// comment holds a closing bracket after the first inner table. Decode
// keeps both references.
func TestDecodeNestedSecretListCommentBracket(t *testing.T) {
	doc := `rows = [[
  {source = "env", var = "A"}, # note: ]
  {source = "file", path = "/x"}
]]
`
	var got struct {
		Rows [][]Secret `toml:"rows"`
	}
	if err := Decode([]byte(doc), &got, stubRegistry(t)); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(got.Rows) != 1 {
		t.Fatalf("got %d outer rows, want 1", len(got.Rows))
	}
	keys := got.Rows[0]
	if len(keys) != 2 {
		t.Fatalf("got %+v, want two references", keys)
	}
	first := keys[0].refs
	second := keys[1].refs
	if len(first) != 1 || first[0].Source() != "env" || len(second) != 1 || second[0].Source() != "file" {
		t.Errorf("rows = %+v, want env then file", keys)
	}
}

// TestDecodeNestedSecretListMultilineInteriorQuote pins a nested secret
// list whose path is a multiline basic string that holds a quote then a
// closing bracket. Decode keeps the reference.
func TestDecodeNestedSecretListMultilineInteriorQuote(t *testing.T) {
	doc := "rows = [[{source = \"file\", path = \"\"\"a \"quoted]\" path\"\"\"}]]\n"
	var got struct {
		Rows [][]Secret `toml:"rows"`
	}
	if err := Decode([]byte(doc), &got, stubRegistry(t)); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if len(got.Rows) != 1 {
		t.Fatalf("got %d outer rows, want 1", len(got.Rows))
	}
	keys := got.Rows[0]
	if len(keys) != 1 {
		t.Fatalf("got %+v, want one reference", keys)
	}
	gotRefs := keys[0].refs
	if len(gotRefs) != 1 || gotRefs[0].Source() != "file" || gotRefs[0].Path() != `a "quoted]" path` {
		t.Errorf("rows = %+v, want file path with an interior quote", keys)
	}
}

// TestDecodeNestedSecretListEmptyInner pins an empty inner list on a nested
// secret list. Decode refuses it as an empty reference list and names rows.
func TestDecodeNestedSecretListEmptyInner(t *testing.T) {
	doc := "rows = [[]]\n"
	var got struct {
		Rows [][]Secret `toml:"rows"`
	}
	err := Decode([]byte(doc), &got, stubRegistry(t))
	if !errors.Is(err, ErrInvalidRef) {
		t.Fatalf("err = %v, want ErrInvalidRef", err)
	}
	if !strings.Contains(err.Error(), "rows: empty reference list") {
		t.Errorf("err = %v, want it to contain %q", err, "rows: empty reference list")
	}
	if strings.Contains(err.Error(), "rows.0") {
		t.Errorf("error carries an element index: %v", err)
	}
}

// TestDecodeMapNestedSecretList pins a nested secret list written as a map
// value. The inner list decodes in document order.
func TestDecodeMapNestedSecretList(t *testing.T) {
	doc := "groups = { a = [[{source = \"env\", var = \"A\"}, {source = \"file\", path = \"/x\"}]] }\n"
	var got struct {
		Groups map[string][][]Secret `toml:"groups"`
	}
	if err := Decode([]byte(doc), &got, stubRegistry(t)); err != nil {
		t.Fatalf("Decode: %v", err)
	}
	inner := got.Groups["a"]
	if len(inner) != 1 {
		t.Fatalf("got %+v, want one inner list", got.Groups)
	}
	keys := inner[0]
	if len(keys) != 2 {
		t.Fatalf("got %+v, want two references", keys)
	}
	first := keys[0].refs
	second := keys[1].refs
	if len(first) != 1 || first[0].Source() != "env" || len(second) != 1 || second[0].Source() != "file" {
		t.Errorf("groups.a = %+v, want env then file", keys)
	}
}
