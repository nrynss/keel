package config

import (
	"bytes"
	"errors"
	"fmt"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

// ErrInvalidRef reports a reference that cannot be parsed or honoured.
var ErrInvalidRef = errors.New("config: invalid reference")

// ReadMode says when a reference resolves.
type ReadMode string

const (
	// ReadAtBoot resolves the reference once, when the settings load. The
	// loader caches the value for the life of the process. It is the
	// default, so a reference without a read key gets it.
	ReadAtBoot ReadMode = "at_boot"

	// ReadAtUse is accepted as a read mode. The loader still resolves it
	// at load and bind caches the value. A rotated file does not change
	// Reveal.
	ReadAtUse ReadMode = "at_use"
)

// Ref names where one secret lives. It carries the source name and the
// locator fields that source reads. A Secret holds an ordered list of them.
//
// A Ref decodes from a TOML table or inline table. Its fields stay
// unexported so that a decoder without the unmarshaler interface enabled
// cannot fill one by reflection. That is what makes the
// EnableUnmarshalerInterface call load bearing, so read a Ref through its
// accessors.
type Ref struct {
	source   string
	path     string
	name     string
	variable string
	command  string
	args     []string
	read     ReadMode
}

// Source returns the name of the source that resolves this reference.
func (r Ref) Source() string { return r.source }

// Path returns the filesystem path the source reads, or the empty string
// when the source takes none.
func (r Ref) Path() string { return r.path }

// Name returns the entry name within a directory the source reads, or the
// empty string when the source takes none.
func (r Ref) Name() string { return r.name }

// Var returns the variable name the source reads, or the empty string when
// the source takes none.
func (r Ref) Var() string { return r.variable }

// Command returns the program the command source runs, or the empty string
// when the source takes none.
func (r Ref) Command() string { return r.command }

// Args returns a copy of the arguments for the command source.
func (r Ref) Args() []string {
	return append([]string(nil), r.args...)
}

// Read returns when this reference resolves.
func (r Ref) Read() ReadMode { return r.read }

// RefConfig is the locator a caller uses to construct a Ref without
// decoding TOML. NewRef copies the fields into a Ref.
type RefConfig struct {
	// Source is the source name, such as env or file.
	Source string
	// Path is the filesystem path the source reads.
	Path string
	// Name is the entry name within a directory the source reads.
	Name string
	// Var is the variable name the source reads.
	Var string
	// Command is the program the command source runs.
	Command string
	// Args is the argument list for the command source.
	Args []string
	// Read says when the reference resolves. Empty means ReadAtBoot.
	Read ReadMode
}

// NewRef returns a Ref with cfg's locator fields. An empty Read becomes
// ReadAtBoot. Args is copied, so the caller can reuse the slice.
func NewRef(cfg RefConfig) Ref {
	read := cfg.Read
	if read == "" {
		read = ReadAtBoot
	}
	var args []string
	if len(cfg.Args) > 0 {
		args = append([]string(nil), cfg.Args...)
	}
	return Ref{
		source:   cfg.Source,
		path:     cfg.Path,
		name:     cfg.Name,
		variable: cfg.Var,
		command:  cfg.Command,
		args:     args,
		read:     read,
	}
}

// locator returns the locator fields the source reads, as space separated
// key=value pairs. The plan printer in this package uses it, and it never
// carries a secret value.
func (r Ref) locator() string {
	var parts []string
	if r.path != "" {
		parts = append(parts, "path="+r.path)
	}
	if r.name != "" {
		parts = append(parts, "name="+r.name)
	}
	if r.variable != "" {
		parts = append(parts, "var="+r.variable)
	}
	if r.command != "" {
		parts = append(parts, "command="+r.command)
	}
	if len(r.args) > 0 {
		parts = append(parts, "args="+strings.Join(r.args, " "))
	}
	return strings.Join(parts, " ")
}

// UnmarshalTOML decodes one reference from a TOML table or inline table.
// go-toml calls it only when the decoder enabled the unmarshaler interface.
func (r *Ref) UnmarshalTOML(data []byte) error {
	refs, err := parseRefs(data, refScan{})
	if err != nil {
		return err
	}
	if len(refs) != 1 {
		return fmt.Errorf("%w: a single reference, not a list", ErrInvalidRef)
	}
	*r = refs[0]
	return nil
}

// refScan is the document context one Secret decodes from. It lets a
// reference failure name the setting key and report the position in the
// file the operator edits. The zero value decodes without context.
type refScan struct {
	doc       []byte // the whole document being decoded
	key       string // dotted key of the setting in the document
	prefixLen int    // synthetic bytes before the fragment on its first line
	fragLine  int    // line the raw value starts at, when found
	fragCol   int    // column the raw value starts at, when found
	found     bool   // whether the raw value was located in the document
}

// refDoc is the decoded shape of one reference. The strict decoder fills it,
// so a misspelled locator key fails by name and position.
type refDoc struct {
	Source  string   `toml:"source"`
	Path    string   `toml:"path"`
	Name    string   `toml:"name"`
	Var     string   `toml:"var"`
	Command string   `toml:"command"`
	Args    []string `toml:"args"`
	Read    string   `toml:"read"`
}

// ref turns a decoded reference into a Ref, refusing an unusable one. The
// failure is a bare detail, and refError attaches the sentinel and the key.
func (d refDoc) ref() (Ref, error) {
	source := strings.TrimSpace(d.Source)
	if source == "" {
		return Ref{}, errors.New("missing source")
	}
	read := ReadAtBoot
	switch d.Read {
	case "", string(ReadAtBoot):
	case string(ReadAtUse):
		read = ReadAtUse
	default:
		return Ref{}, fmt.Errorf("unknown read mode %q", d.Read)
	}
	return Ref{
		source:   source,
		path:     d.Path,
		name:     d.Name,
		variable: d.Var,
		command:  d.Command,
		args:     d.Args,
		read:     read,
	}, nil
}

// parseRefs decodes the raw bytes of a secret setting into one or more
// references, in document order. The bytes are a table body, an inline
// table, or an inline array of tables. scan carries the document context a
// failure reports, and its zero value means no context.
func parseRefs(raw []byte, scan refScan) ([]Ref, error) {
	body := bytes.TrimSpace(raw)
	if len(body) == 0 {
		return nil, refError(errors.New("empty reference"), scan)
	}
	scan.fragLine, scan.fragCol = fragmentLead(raw, scan.fragLine, scan.fragCol)
	switch body[0] {
	case '{':
		var wrap struct {
			V refDoc `toml:"v"`
		}
		scan.prefixLen = len("v = ")
		if err := decodeRefDoc(append([]byte("v = "), body...), &wrap, scan); err != nil {
			return nil, err
		}
		ref, err := wrap.V.ref()
		if err != nil {
			return nil, refError(err, scan)
		}
		return []Ref{ref}, nil
	case '[':
		var wrap struct {
			V []refDoc `toml:"v"`
		}
		scan.prefixLen = len("v = ")
		if err := decodeRefDoc(append([]byte("v = "), body...), &wrap, scan); err != nil {
			return nil, err
		}
		return refsFromDocs(wrap.V, scan)
	default:
		if looksLikeValue(body) {
			return nil, ErrInlineSecret
		}
		var d refDoc
		if err := decodeRefDoc(body, &d, scan); err != nil {
			return nil, err
		}
		ref, err := d.ref()
		if err != nil {
			return nil, refError(err, scan)
		}
		return []Ref{ref}, nil
	}
}

// refsFromDocs turns decoded references into Refs, in order.
func refsFromDocs(docs []refDoc, scan refScan) ([]Ref, error) {
	if len(docs) == 0 {
		return nil, refError(errors.New("empty reference list"), scan)
	}
	refs := make([]Ref, 0, len(docs))
	for _, d := range docs {
		ref, err := d.ref()
		if err != nil {
			return nil, refError(err, scan)
		}
		refs = append(refs, ref)
	}
	return refs, nil
}

// decodeRefDoc decodes a reference document with unknown keys refused. The
// failure keeps the offending key and carries the position in the document.
func decodeRefDoc(doc []byte, dst any, scan refScan) error {
	err := toml.NewDecoder(bytes.NewReader(doc)).DisallowUnknownFields().Decode(dst)
	if err != nil {
		return refError(err, scan)
	}
	return nil
}

// looksLikeValue reports whether raw is a bare TOML value rather than a
// reference. A secret setting that carries one is refused by name later.
func looksLikeValue(raw []byte) bool {
	switch string(raw) {
	case "true", "false":
		return true
	}
	switch raw[0] {
	case '"', '\'':
		return true
	}
	return raw[0] >= '0' && raw[0] <= '9'
}

// refError turns a parser failure inside a reference into a message that
// names the setting key and places every offending key at its position in
// the document. The scan carries the setting key and the file position,
// including a secret the decoder creates. A failure with no position keeps
// the text the parser wrote.
func refError(err error, scan refScan) error {
	key := ""
	if scan.key != "" {
		key = scan.key + ": "
	}
	var strict *toml.StrictMissingError
	if errors.As(err, &strict) {
		parts := make([]string, 0, len(strict.Errors))
		for _, e := range strict.Errors {
			row, col := e.Position()
			row, col = docPosition(scan, row, col)
			parts = append(parts, fmt.Sprintf("%q at line %d, column %d", strings.Join(e.Key(), "."), row, col))
		}
		return fmt.Errorf("%w: %sunknown key %s", ErrInvalidRef, key, strings.Join(parts, ", "))
	}
	var de *toml.DecodeError
	if errors.As(err, &de) {
		row, col := de.Position()
		row, col = docPosition(scan, row, col)
		msg := strings.TrimPrefix(de.Error(), "toml: ")
		return fmt.Errorf("%w: %s%s at line %d, column %d", ErrInvalidRef, key, msg, row, col)
	}
	return fmt.Errorf("%w: %s%v", ErrInvalidRef, key, err)
}

// docPosition converts a position inside the re-decoded reference bytes to
// the position in the document. The fragment starts at fragLine and fragCol,
// and prefixLen counts the synthetic bytes before it on the first line.
func docPosition(scan refScan, row, col int) (int, int) {
	if !scan.found {
		return row, col
	}
	line := scan.fragLine + row - 1
	if row > 1 {
		return line, col
	}
	return line, scan.fragCol + col - 1 - scan.prefixLen
}

// fragmentLead shifts a fragment start past the leading blanks the trimmed
// body dropped, so the first line keeps its true column.
func fragmentLead(raw []byte, line, col int) (int, int) {
	lead := raw[:len(raw)-len(bytes.TrimLeft(raw, " \t\r\n"))]
	line += bytes.Count(lead, []byte("\n"))
	if nl := bytes.LastIndexByte(lead, '\n'); nl >= 0 {
		col = len(lead) - nl
	} else {
		col += len(lead)
	}
	return line, col
}

// locateFragment finds the raw value bytes in the document, so a failure can
// report where the setting sits in the file. It looks behind the table
// header or the key the setting is written under first, then for the value
// alone. A match must start its own line, after any leading blanks and
// header brackets. A line inside a comment or a multi-line string is not
// the setting. One that shares a line with something else is taken only
// when no line carries the text alone. It reports false when the bytes
// cannot be found at all.
func locateFragment(scan refScan, raw []byte) (int, int, bool) {
	if len(scan.doc) == 0 || len(raw) == 0 {
		return 0, 0, false
	}
	needles := locateNeedles(scan.key, raw)
	for _, needle := range needles {
		if at, ok := lineStart(scan.doc, needle); ok {
			return fragmentAt(scan.doc, at+len(needle)-len(raw))
		}
	}
	for _, needle := range needles {
		if at := bytes.Index(scan.doc, needle); at >= 0 {
			return fragmentAt(scan.doc, at+len(needle)-len(raw))
		}
	}
	return 0, 0, false
}

// lineStart finds the first occurrence of needle that starts a line after
// any leading blanks and header brackets, and that does not sit inside a
// string.
func lineStart(doc, needle []byte) (int, bool) {
	for from := 0; ; {
		at := bytes.Index(doc[from:], needle)
		if at < 0 {
			return 0, false
		}
		at += from
		if leadIsBlank(doc, at) && !insideString(doc, at) {
			return at, true
		}
		from = at + 1
	}
}

// tomlLex names the lexical state of TOML quoting and comments.
type tomlLex int

const (
	tomlPlain tomlLex = iota
	tomlBasic
	tomlLiteral
	tomlMultiBasic
	tomlMultiLiteral
	tomlComment
)

// nextStringState steps one byte of TOML quoting and comments.
// extra is how many following bytes the step already consumed.
// A multiline closer is a run of three, four, or five quotes.
func nextStringState(doc []byte, i int, state tomlLex) (tomlLex, int) {
	c := doc[i]
	switch state {
	case tomlComment:
		if c == '\n' {
			return tomlPlain, 0
		}
		return tomlComment, 0
	case tomlBasic:
		switch c {
		case '\\':
			return tomlBasic, 1
		case '"':
			return tomlPlain, 0
		}
		return tomlBasic, 0
	case tomlLiteral:
		if c == '\'' {
			return tomlPlain, 0
		}
		return tomlLiteral, 0
	case tomlMultiBasic:
		switch {
		case c == '\\':
			return tomlMultiBasic, 1
		case c == '"':
			n := quoteRun(doc, i, '"')
			if n >= 3 {
				return tomlPlain, n - 1
			}
			return tomlMultiBasic, n - 1
		}
		return tomlMultiBasic, 0
	case tomlMultiLiteral:
		if c == '\'' {
			n := quoteRun(doc, i, '\'')
			if n >= 3 {
				return tomlPlain, n - 1
			}
			return tomlMultiLiteral, n - 1
		}
		return tomlMultiLiteral, 0
	default:
		switch {
		case c == '#':
			return tomlComment, 0
		case bytes.HasPrefix(doc[i:], []byte(`"""`)):
			return tomlMultiBasic, 2
		case bytes.HasPrefix(doc[i:], []byte("'''")):
			return tomlMultiLiteral, 2
		case c == '"':
			return tomlBasic, 0
		case c == '\'':
			return tomlLiteral, 0
		}
		return tomlPlain, 0
	}
}

// quoteRun counts consecutive quotes at i, at most five.
func quoteRun(doc []byte, i int, q byte) int {
	n := 0
	for n < 5 && i+n < len(doc) && doc[i+n] == q {
		n++
	}
	return n
}

// insideString reports whether at sits inside a string, so a line inside a
// multi-line string is not taken for the setting. It walks nextStringState
// from the start of the document.
func insideString(doc []byte, at int) bool {
	state := tomlPlain
	for i := 0; i < at && i < len(doc); i++ {
		var extra int
		state, extra = nextStringState(doc, i, state)
		i += extra
	}
	return state != tomlPlain && state != tomlComment
}

// leadIsBlank reports whether only blanks and header brackets sit between
// the start of the line and at.
func leadIsBlank(doc []byte, at int) bool {
	start := bytes.LastIndexByte(doc[:at], '\n') + 1
	for _, b := range doc[start:at] {
		switch b {
		case ' ', '\t', '[', ']':
		default:
			return false
		}
	}
	return true
}

// locateNeedles lists the byte patterns a raw value is searched behind, most
// specific first. A table body sits under its header, an inline value under
// its key, and the bare value is the last resort.
func locateNeedles(key string, raw []byte) [][]byte {
	if key == "" {
		return [][]byte{raw}
	}
	last := key[strings.LastIndex(key, ".")+1:]
	return [][]byte{
		[]byte(fmt.Sprintf("[%s]\n%s", key, raw)),
		[]byte(fmt.Sprintf("[[%s]]\n%s", key, raw)),
		[]byte(fmt.Sprintf("%s = %s", last, raw)),
		raw,
	}
}

// fragmentAt returns the one based line and column of an offset in a
// document.
func fragmentAt(doc []byte, at int) (int, int, bool) {
	line := 1 + bytes.Count(doc[:at], []byte("\n"))
	col := at + 1
	if nl := bytes.LastIndexByte(doc[:at], '\n'); nl >= 0 {
		col = at - nl
	}
	return line, col, true
}
