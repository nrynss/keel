// Package config loads an application's settings from a TOML file, and
// resolves its secrets from wherever each one lives.
//
// Settings sit inline in the file. A secret is a reference, never a value:
// the file says which source holds the secret and how to find it there, and
// the loader reads the value at load or at use. No secret value is ever
// written in the file, so a settings file is safe to keep in a repository.
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
