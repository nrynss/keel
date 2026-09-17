// Package config loads an application's settings from a TOML file, and
// resolves its secrets from wherever each one lives.
//
// Settings sit inline in the file. A secret is a reference, never a value:
// the file says which source holds the secret and how to find it there.
// The loader trial resolves every secret at load, including a reference
// marked at_use, and that trial feeds the plan. An at_boot secret returns
// the cached trial value. An at_use secret resolves again on each Reveal,
// so rotation takes effect without a restart. The trial winner decides the
// mode for the whole list. No secret value is ever written in the file, so
// a settings file is safe to keep in a repository.
//
// # The reference shape
//
// A secret setting is a reference table. The source names the mechanism, and
// the remaining keys locate the value within it.
//
//	[secrets.provider_api_key]
//	source = "env_file"
//	path   = "/etc/app/env"
//	var    = "PROVIDER_API_KEY"
//	read   = "at_use"
//
// An ordered list of references is allowed, so one setting can name a
// fallback. The file always writes the order out, and nothing falls back
// implicitly. A stale environment variable that silently beats an edited file
// is the worst failure this package can have.
//
// # What the loader refuses
//
// The parser is strict. An unknown key, an unknown source name, and a literal
// value where a reference belongs all stop the load and name the key. A load
// that fails at boot names the fault while an operator is watching, rather
// than surfacing it later as a refused call.
//
// # Nothing prints a value
//
// A resolved Secret has no formatting path that reveals it. String, Format,
// the JSON and text marshallers, and the slog value all print a fixed
// placeholder. Reveal is the one way to read the value, and it reads as a
// deliberate act at the call site.
//
// # Precedence
//
// Load fills a settings struct in one order. Defaults are the values already
// in the struct. The file overlays them. Environment overrides overlay the
// file, for non-secret settings only. Explicit flags win last.
//
// The override key is the field path in upper case, with dots and hyphens
// turned into underscores. render.max_seconds reads RENDER_MAX_SECONDS.
// There is no second table and no application prefix.
//
// # File choice
//
// Config.Path names the file. When it is empty, PathVar is looked up and its
// value is the path. When that is empty too, Search is tried in order. A
// missing file is not an error. A present file that cannot be read or parsed
// is fatal.
//
// # Required fields
//
// A field tagged config:"required" cannot stay zero. Load fails with the
// key, its source, and its locator when no layer supplies a value.
//
// # The plan
//
// Load returns a Plan. String prints one tab separated line per key: name,
// source, locator, and whether it resolved. No line carries a value. Dump
// writes the effective settings as TOML, with every secret replaced by its
// plan line.
//
// # No environment reads
//
// The package never reads the process environment. The caller injects the
// lookup the loader uses for the override layer, so a test drives every layer
// without touching the process.
//
// # The parser
//
// The TOML parser is github.com/pelletier/go-toml/v2. Its strict decoder
// reports an unknown field with the offending key and its position, which is
// the property this package is built on. Ref decoding depends on the
// decoder's unmarshaler interface, which is off unless the caller enables it
// with EnableUnmarshalerInterface.
package config
