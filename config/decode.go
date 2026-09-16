package config

import (
	"bytes"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"

	"github.com/pelletier/go-toml/v2"
	"github.com/pelletier/go-toml/v2/unstable"
)

// ErrInvalidDest reports a Decode target that is not a pointer to a struct.
var ErrInvalidDest = errors.New("config: decode target must be a pointer to a struct")

// ErrUnknownKey reports a key in the file that the settings struct has no
// field for.
var ErrUnknownKey = errors.New("config: unknown key")

// ErrMalformed reports a file that does not parse, or a value whose type the
// settings struct cannot hold.
var ErrMalformed = errors.New("config: malformed settings")

// secretType is the field type that marks a setting as a secret.
var secretType = reflect.TypeOf(Secret{})

// Decode parses a TOML document into dst, a pointer to the application's
// settings struct.
//
// A field of type Secret holds a reference, and the document holds no value
// for it. Decode refuses a literal value there, unknown keys, unknown source
// names, and a malformed reference. Every refusal names the setting key,
// including a secret the decoder creates. reg supplies the names a
// reference may use as its source.
//
// Decode parses only. It resolves nothing, so every Secret it fills still
// reports ErrUnresolved until a loader resolves it.
func Decode(data []byte, dst any, reg *Registry) error {
	rv := reflect.ValueOf(dst)
	if rv.Kind() != reflect.Pointer || rv.IsNil() {
		return ErrInvalidDest
	}
	el := rv.Elem()
	if el.Kind() != reflect.Struct {
		return ErrInvalidDest
	}
	var tree map[string]any
	if err := toml.Unmarshal(data, &tree); err != nil {
		return fmt.Errorf("%w: %s", ErrMalformed, strings.TrimSpace(err.Error()))
	}
	if err := checkSecrets(el.Type(), tree, nil, reg); err != nil {
		return err
	}
	if err := checkRefKeys(el.Type(), data); err != nil {
		return err
	}
	stashed, decodeDoc, err := stashSecretArrays(el.Type(), data)
	if err != nil {
		return err
	}
	markSecrets(el, nil, decodeDoc)
	defer clearSecretScan(el)
	dec := toml.NewDecoder(bytes.NewReader(decodeDoc)).DisallowUnknownFields().EnableUnmarshalerInterface()
	if err := dec.Decode(dst); err != nil {
		return decodeError(err)
	}
	injectSecretArrays(el, nil, stashed)
	return nil
}

// checkSecrets walks the settings type beside the parsed document. It
// refuses a literal value on a Secret field and a reference whose source
// name is not registered, naming the key in both cases. It follows every
// shape a Secret can sit behind, including the embedded structs the decoder
// flattens.
func checkSecrets(t reflect.Type, node any, prefix []string, reg *Registry) error {
	if t == secretType {
		return checkSecretNode(node, prefix, reg)
	}
	switch t.Kind() {
	case reflect.Pointer:
		return checkSecrets(t.Elem(), node, prefix, reg)
	case reflect.Slice, reflect.Array:
		if isSecretList(t) {
			return checkSecretList(node, prefix, reg)
		}
		list, ok := node.([]any)
		if !ok {
			return nil
		}
		for _, item := range list {
			// An element index is not part of a setting key, so the
			// message names the setting alone.
			if err := checkSecrets(t.Elem(), item, prefix, reg); err != nil {
				return err
			}
		}
	case reflect.Map:
		tree, ok := node.(map[string]any)
		if !ok {
			return nil
		}
		for _, key := range sortedKeys(tree) {
			if err := checkSecrets(t.Elem(), tree[key], appendPath(prefix, key), reg); err != nil {
				return err
			}
		}
	case reflect.Struct:
		tree, ok := node.(map[string]any)
		if !ok {
			return nil
		}
		for i := 0; i < t.NumField(); i++ {
			field := t.Field(i)
			if field.Anonymous && embeddedFlat(field) {
				if err := checkSecrets(field.Type, node, prefix, reg); err != nil {
					return err
				}
				continue
			}
			key, ok := fieldKey(field)
			if !ok {
				continue
			}
			child, ok := lookupFold(tree, key)
			if !ok {
				continue
			}
			if err := checkSecrets(field.Type, child, appendPath(prefix, key), reg); err != nil {
				return err
			}
		}
	}
	return nil
}

// markSecrets replaces every Secret reachable from v with a fresh one that
// carries the document and the dotted key, so a reference failure names the
// key and reports its position in the file. The fresh value also drops the
// references a previous decode left, so decoding twice does not accumulate.
func markSecrets(v reflect.Value, prefix []string, doc []byte) {
	switch v.Kind() {
	case reflect.Pointer:
		if v.IsNil() {
			if v.Type().Elem() != secretType || !v.CanSet() {
				return
			}
			v.Set(reflect.New(secretType))
		}
		markSecrets(v.Elem(), prefix, doc)
	case reflect.Struct:
		if v.Type() == secretType {
			if v.CanSet() {
				v.Set(reflect.ValueOf(Secret{scan: refScan{doc: doc, key: strings.Join(prefix, ".")}}))
			}
			return
		}
		for i := 0; i < v.NumField(); i++ {
			field := v.Type().Field(i)
			if field.Anonymous && embeddedFlat(field) {
				markSecrets(v.Field(i), prefix, doc)
				continue
			}
			key, ok := fieldKey(field)
			if !ok {
				continue
			}
			markSecrets(v.Field(i), appendPath(prefix, key), doc)
		}
	case reflect.Slice, reflect.Array:
		for i := 0; i < v.Len(); i++ {
			markSecrets(v.Index(i), appendPath(prefix, strconv.Itoa(i)), doc)
		}
	case reflect.Map:
		for _, key := range v.MapKeys() {
			item := reflect.New(v.Type().Elem()).Elem()
			item.Set(v.MapIndex(key))
			markSecrets(item, appendPath(prefix, key.String()), doc)
			v.SetMapIndex(key, item)
		}
	}
}

// clearSecretScan drops the decode context from every Secret reachable from
// v, so a decoded settings struct holds no document bytes.
func clearSecretScan(v reflect.Value) {
	switch v.Kind() {
	case reflect.Pointer, reflect.Interface:
		if !v.IsNil() {
			clearSecretScan(v.Elem())
		}
	case reflect.Struct:
		if v.Type() == secretType {
			if v.CanSet() && v.CanInterface() {
				s := v.Interface().(Secret)
				s.scan = refScan{}
				v.Set(reflect.ValueOf(s))
			}
			return
		}
		for i := 0; i < v.NumField(); i++ {
			clearSecretScan(v.Field(i))
		}
	case reflect.Slice, reflect.Array:
		for i := 0; i < v.Len(); i++ {
			clearSecretScan(v.Index(i))
		}
	case reflect.Map:
		for _, key := range v.MapKeys() {
			item := reflect.New(v.Type().Elem()).Elem()
			item.Set(v.MapIndex(key))
			clearSecretScan(item)
			v.SetMapIndex(key, item)
		}
	}
}

// secretArray is one inline array of references written on a list setting.
// go-toml hands each slice element only the opening brace of its inline
// table, so this shape cannot decode through the unmarshaler interface.
// The array is read here instead, and injected after the decode.
type secretArray struct {
	key   []string
	refs  []Secret
	start int
	end   int
}

// stashSecretArrays reads every inline array of references written on a
// list setting, and returns it beside a copy of the document with each
// array emptied. The copy stays a valid document, and line breaks stay in
// place, so the strict decoder fills an empty list at the right position
// and the references are injected into it afterwards. The record's path
// carries the element index of each enclosing array table, restarted for
// a new parent element. A fixed size array takes the references only when
// the document writes exactly that many.
func stashSecretArrays(t reflect.Type, doc []byte) ([]secretArray, []byte, error) {
	var parser unstable.Parser
	parser.Reset(doc)
	var table []string
	open := newOpenTables()
	var pending []pendingTable
	var found []secretArray
	for parser.NextExpression() {
		expr := parser.Expression()
		switch expr.Kind {
		case unstable.Table, unstable.ArrayTable:
			table = tablePath(table, expr, open)
			if err := checkRefTableBody(t, table, &pending); err != nil {
				return nil, nil, err
			}
		case unstable.KeyValue:
			if len(pending) > 0 {
				continue
			}
			if err := stashValue(t, tableParts(table, expr), expr, &parser, doc, &found); err != nil {
				return nil, nil, err
			}
		default:
		}
	}
	if parser.Error() != nil || len(found) == 0 {
		return nil, doc, nil
	}
	blanked := append([]byte(nil), doc...)
	for _, array := range found {
		emptyArraySpan(blanked, array.start, array.end)
	}
	return found, blanked, nil
}

// valueSpan returns the span of one assignment's value in the document.
// The parser reports the whole assignment, so the value starts after the
// first equals sign outside a quoted key. A value written inside an
// array carries no range of its own, so the assignment is the only source
// for this span. It reports false rather than a guessed span.
func valueSpan(parser *unstable.Parser, expr *unstable.Node, doc []byte) (int, int, bool) {
	text := parser.Raw(expr.Raw)
	at := valueOffset(text)
	if at < 0 {
		return 0, 0, false
	}
	rest := text[at:]
	lead := len(rest) - len(bytes.TrimLeft(rest, " \t"))
	body := bytes.TrimRight(rest[lead:], " \t\r")
	start := int(expr.Raw.Offset) + at + lead
	end := start + len(body)
	if end > len(doc) || !bytes.Equal(doc[start:end], body) {
		return 0, 0, false
	}
	return start, end, true
}

// stashValue records the inline arrays written inside one assignment. The
// value is the array itself, an inline table holding one deeper, or an
// inline array of structs holding one deeper. The walk descends until it
// reaches a list setting. A fixed size array takes the references only when
// the document writes exactly that many.
func stashValue(
	t reflect.Type,
	parts []string,
	expr *unstable.Node,
	parser *unstable.Parser,
	doc []byte,
	found *[]secretArray,
) error {
	value := expr.Value()
	if value.Kind == unstable.InlineTable {
		return stashInlineTable(t, parts, value, parser, doc, found)
	}
	if value.Kind != unstable.Array {
		return nil
	}
	target, prefix, settled, ok := secretTarget(t, parts)
	if ok && isSecretList(target) {
		start, end, ok := valueSpan(parser, expr, doc)
		if !ok {
			return nil
		}
		refs, err := parseArraySpan(doc, start, end, prefix)
		if err != nil {
			return err
		}
		if target.Kind() == reflect.Array && target.Len() != len(refs) {
			return fmt.Errorf(
				"%w: %s: the setting holds %d, the document writes %d references",
				ErrInvalidRef,
				strings.Join(prefix, "."),
				target.Len(),
				len(refs),
			)
		}
		*found = append(*found, secretArray{key: settled, refs: refs, start: start, end: end})
		return nil
	}
	return stashInlineArray(t, parts, value, parser, doc, found)
}

// stashInlineTable records secret lists written inside one inline table.
func stashInlineTable(
	t reflect.Type,
	parts []string,
	value *unstable.Node,
	parser *unstable.Parser,
	doc []byte,
	found *[]secretArray,
) error {
	children := value.Children()
	for children.Next() {
		child := children.Node()
		if child.Kind != unstable.KeyValue {
			continue
		}
		if err := stashValue(t, tableParts(parts, child), child, parser, doc, found); err != nil {
			return err
		}
	}
	return nil
}

// stashInlineArray records secret lists written inside an inline array of
// structs, maps, or nested arrays. An array element that is a secret list
// is recorded at the settled path. Other elements are walked with their
// index on the path, so a nested list is recorded at the slice the
// decoder fills.
func stashInlineArray(
	t reflect.Type,
	parts []string,
	value *unstable.Node,
	parser *unstable.Parser,
	doc []byte,
	found *[]secretArray,
) error {
	children := value.Children()
	i := 0
	for children.Next() {
		elem := children.Node()
		if elem.Kind == unstable.Comment {
			continue
		}
		elemParts := appendPath(parts, strconv.Itoa(i))
		switch elem.Kind {
		case unstable.InlineTable:
			if err := stashInlineTable(t, elemParts, elem, parser, doc, found); err != nil {
				return err
			}
		case unstable.Array:
			target, prefix, settled, ok := secretTarget(t, elemParts)
			if ok && isSecretList(target) {
				if err := stashNestedSecretList(target, prefix, settled, elem, doc, found); err != nil {
					return err
				}
				break
			}
			if err := stashInlineArray(t, elemParts, elem, parser, doc, found); err != nil {
				return err
			}
		default:
			continue
		}
		i++
	}
	return nil
}

// stashNestedSecretList records one secret list written as an array
// element. go-toml leaves Array.Raw unset, so the span is rebuilt from
// the first child back to the opening bracket.
func stashNestedSecretList(
	target reflect.Type,
	prefix []string,
	settled []string,
	elem *unstable.Node,
	doc []byte,
	found *[]secretArray,
) error {
	start, end, ok := nestedArraySpan(elem, doc)
	if !ok {
		return nil
	}
	refs, err := parseArraySpan(doc, start, end, prefix)
	if err != nil {
		return err
	}
	if target.Kind() == reflect.Array && target.Len() != len(refs) {
		return fmt.Errorf(
			"%w: %s: the setting holds %d, the document writes %d references",
			ErrInvalidRef,
			strings.Join(prefix, "."),
			target.Len(),
			len(refs),
		)
	}
	*found = append(*found, secretArray{key: settled, refs: refs, start: start, end: end})
	return nil
}

// valueOffset returns where a key value's value starts, which is the first
// equals sign outside a quoted key.
func valueOffset(text []byte) int {
	var quote byte
	for i := 0; i < len(text); i++ {
		c := text[i]
		switch {
		case quote == '"':
			if c == '\\' {
				i++
				continue
			}
			if c == '"' {
				quote = 0
			}
		case quote == '\'':
			if c == '\'' {
				quote = 0
			}
		case c == '"' || c == '\'':
			quote = c
		case c == '=':
			return i + 1
		}
	}
	return -1
}

// parseArraySpan reads the references of one inline array. The array text
// is the value alone, and the scan context places every failure at its
// position in the document. Each element is one secret of the list.
func parseArraySpan(doc []byte, start, end int, prefix []string) ([]Secret, error) {
	line, col, _ := fragmentAt(doc, start)
	scan := refScan{
		doc:      doc,
		key:      strings.Join(prefix, "."),
		fragLine: line,
		fragCol:  col,
		found:    true,
	}
	refs, err := parseRefs(doc[start:end], scan)
	if err != nil {
		return nil, err
	}
	list := make([]Secret, 0, len(refs))
	for _, ref := range refs {
		list = append(list, Secret{refs: []Ref{ref}})
	}
	return list, nil
}

// emptyArraySpan blanks the inside of one inline array, so the strict
// decoder fills an empty list there. The brackets and every line break
// stay in place, so the copy keeps its byte count and its line count, and
// a failure anywhere after the array still reports the position in the
// file the operator edits.
func emptyArraySpan(doc []byte, start, end int) {
	for i := start + 1; i < end-1; i++ {
		if doc[i] != '\n' {
			doc[i] = ' '
		}
	}
}

// injectSecretArrays writes the references read from each inline array into
// the list setting it was written on. The strict decode left that list
// empty, because the decode copy carries an emptied array. Each list is
// matched by the path the type walk settles, including the element index
// of each enclosing array table. The walk order does not matter.
func injectSecretArrays(v reflect.Value, prefix []string, stashed []secretArray) []secretArray {
	switch v.Kind() {
	case reflect.Pointer:
		if !v.IsNil() {
			return injectSecretArrays(v.Elem(), prefix, stashed)
		}
	case reflect.Struct:
		if v.Type() == secretType {
			return stashed
		}
		for i := 0; i < v.NumField(); i++ {
			field := v.Type().Field(i)
			if field.Anonymous && embeddedFlat(field) {
				stashed = injectSecretArrays(v.Field(i), prefix, stashed)
				continue
			}
			key, ok := fieldKey(field)
			if !ok {
				continue
			}
			stashed = injectSecretArrays(v.Field(i), appendPath(prefix, key), stashed)
		}
	case reflect.Slice, reflect.Array:
		if at := stashIndex(stashed, prefix); at >= 0 {
			fillSecretList(v, stashed[at].refs)
			return append(stashed[:at], stashed[at+1:]...)
		}
		for i := 0; i < v.Len(); i++ {
			stashed = injectSecretArrays(v.Index(i), appendPath(prefix, strconv.Itoa(i)), stashed)
		}
	case reflect.Map:
		for _, key := range v.MapKeys() {
			item := reflect.New(v.Type().Elem()).Elem()
			item.Set(v.MapIndex(key))
			stashed = injectSecretArrays(item, appendPath(prefix, key.String()), stashed)
			v.SetMapIndex(key, item)
		}
	}
	return stashed
}

// stashIndex finds the stashed array written at one document path, so the
// walk order does not matter. Two lists of the same setting differ by an
// element index, and the walk reaches their elements in the order the
// document wrote them.
func stashIndex(stashed []secretArray, key []string) int {
	for i, array := range stashed {
		if samePath(array.key, key) {
			return i
		}
	}
	return -1
}

// samePath reports whether two dotted keys name the same setting.
func samePath(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// fillSecretList puts the references read from an inline array into the
// list setting they were written on, in the order the array wrote them. A
// fixed size array takes them only when it holds exactly that many, which
// the stash check has already established.
func fillSecretList(v reflect.Value, refs []Secret) {
	if !v.CanSet() {
		return
	}
	var out reflect.Value
	if v.Kind() == reflect.Array {
		if v.Len() != len(refs) {
			return
		}
		out = reflect.New(v.Type()).Elem()
	} else {
		out = reflect.MakeSlice(v.Type(), len(refs), len(refs))
	}
	for i, ref := range refs {
		elem := out.Index(i)
		if elem.Kind() == reflect.Pointer {
			held := reflect.New(elem.Type().Elem())
			held.Elem().Set(reflect.ValueOf(ref))
			elem.Set(held)
			continue
		}
		elem.Set(reflect.ValueOf(ref))
	}
	v.Set(out)
}

// checkRefKeys validates every secret reference by walking the document
// beside the settings type. It refuses an unknown locator key, a locator
// of the wrong type, and an unknown read mode. It reports the setting key
// and the position in the file, including a secret the decoder creates.
func checkRefKeys(t reflect.Type, doc []byte) error {
	var parser unstable.Parser
	parser.Reset(doc)
	var table []string
	open := newOpenTables()
	var pending []pendingTable
	for parser.NextExpression() {
		expr := parser.Expression()
		switch expr.Kind {
		case unstable.Table, unstable.ArrayTable:
			if err := checkPendingBody(pending, doc); err != nil {
				return err
			}
			table = tablePath(table, expr, open)
			if err := checkRefTableBody(t, table, &pending); err != nil {
				return err
			}
		case unstable.KeyValue:
			if err := checkRefKeyValue(t, table, expr, &parser, doc, &pending); err != nil {
				return err
			}
		default:
		}
	}
	if err := parser.Error(); err != nil {
		return nil
	}
	return checkPendingBody(pending, doc)
}

// openTables tracks the element index each array table is open at, keyed by
// the header path that opened it.
type openTables struct {
	at   map[string]int
	next map[string]int
}

// newOpenTables returns the empty state a header walk starts from.
func newOpenTables() *openTables {
	return &openTables{at: make(map[string]int), next: make(map[string]int)}
}

// tablePath appends one table header to the path, and adds the element index
// of every array table the header sits inside. A new element restarts the
// lists inside it, so a nested list's index is local to that element. No
// index reaches a message.
func tablePath(table []string, expr *unstable.Node, open *openTables) []string {
	keys := expr.Key()
	named := make([]string, 0, len(table)+1)
	for keys.Next() {
		named = append(named, string(keys.Node().Data))
	}
	// A header closes every array table that is not its own ancestor.
	for name := range open.at {
		if !headerPrefix(name, named) {
			delete(open.at, name)
		}
	}
	path := table[:0]
	for i, part := range named {
		path = append(path, part)
		if expr.Kind == unstable.ArrayTable && i == len(named)-1 {
			// This header opens its own array table, so its index is
			// appended below rather than read from the open state.
			continue
		}
		if at, ok := open.at[strings.Join(named[:i+1], ".")]; ok {
			path = append(path, strconv.Itoa(at))
		}
	}
	if expr.Kind != unstable.ArrayTable {
		return path
	}
	name := strings.Join(named, ".")
	at := open.next[name]
	open.next[name]++
	open.at[name] = at
	// A new element of an array table restarts the lists inside it, so
	// every counter below this header starts again.
	for other := range open.next {
		if other != name && strings.HasPrefix(other, name+".") {
			delete(open.next, other)
		}
	}
	return append(path, strconv.Itoa(at))
}

// headerPrefix reports whether a header path names an ancestor of another
// header, or the same table. Whole segments are compared, so a prefix of one
// key never matches the start of another.
func headerPrefix(name string, parts []string) bool {
	joined := ""
	for i, part := range parts {
		if i > 0 {
			joined += "."
		}
		joined += part
		if joined == name {
			return true
		}
	}
	return false
}

// pendingTable holds a table header that may own the key values that
// follow it. The header names a secret setting directly, so each later
// key is a locator key of that setting. start and end span the body in
// the file, so a typed fault is read here rather than only by the decoder.
type pendingTable struct {
	target reflect.Type
	prefix []string
	start  int
	end    int
	body   bool
}

// checkRefTableBody notes a table header that names a secret setting. A
// table body holds the locator keys of one reference, so the key values
// that follow belong to that setting until the next header. An array
// table header carries the index of the element it opens, so its body
// resolves to the one secret that element holds.
func checkRefTableBody(t reflect.Type, table []string, pending *[]pendingTable) error {
	target, prefix, _, ok := secretTarget(t, table)
	if !ok || target != secretType {
		*pending = nil
		return nil
	}
	*pending = []pendingTable{{target: target, prefix: prefix}}
	return nil
}

// checkRefKeyValue checks one assignment against the settings type. A secret
// written as an inline table or an inline array of references is read here.
// An inline table, an inline array of structs, a list of maps, or a nested
// array is walked even when its own path is not a secret. A nested secret
// list sitting as an array element is checked the same way. A locator typo
// inside it names the setting and the file position.
func checkRefKeyValue(
	t reflect.Type,
	table []string,
	expr *unstable.Node,
	parser *unstable.Parser,
	doc []byte,
	pending *[]pendingTable,
) error {
	if len(*pending) > 0 {
		name, row, col := keyNameAt(expr, parser, doc)
		if !isRefDocKey(name) {
			head := (*pending)[0]
			return fmt.Errorf(
				"%w: %sunknown key %q at line %d, column %d",
				ErrInvalidRef,
				secretPath(head.prefix),
				name,
				row,
				col,
			)
		}
		recordPendingSpan(pending, expr)
		return nil
	}
	parts := tableParts(table, expr)
	value := expr.Value()
	target, prefix, _, ok := secretTarget(t, parts)
	if ok && target == secretType {
		if value.Kind == unstable.InlineTable {
			return checkRefInlineTable(target, prefix, expr, parser, doc)
		}
		if value.Kind == unstable.Array {
			return parseRefAssignment(expr, parser, doc, prefix)
		}
		return nil
	}
	if value.Kind == unstable.InlineTable {
		return checkRefInlineValue(t, parts, value, parser, doc)
	}
	if value.Kind == unstable.Array {
		if ok && isSecretList(target) {
			return checkRefArray(target, prefix, expr, parser, doc)
		}
		return checkRefInlineArray(t, parts, value, parser, doc)
	}
	return nil
}

// recordPendingSpan extends the pending table body to cover one assignment,
// so the body can be read after the header's keys end.
func recordPendingSpan(pending *[]pendingTable, expr *unstable.Node) {
	if len(*pending) == 0 || expr.Raw.Length == 0 {
		return
	}
	start := int(expr.Raw.Offset)
	end := start + int(expr.Raw.Length)
	head := &(*pending)[0]
	if !head.body {
		head.start = start
		head.end = end
		head.body = true
		return
	}
	if start < head.start {
		head.start = start
	}
	if end > head.end {
		head.end = end
	}
}

// checkPendingBody reads the collected table body of a secret setting. A
// locator of the wrong type and an unknown read mode fail with the setting
// key and the position in the file, including a secret the decoder creates.
func checkPendingBody(pending []pendingTable, doc []byte) error {
	if len(pending) == 0 || !pending[0].body {
		return nil
	}
	head := pending[0]
	return parseRefSpan(doc, head.start, head.end, head.prefix)
}

// parseRefSpan decodes one reference body from the document. A typed fault
// and an unknown read mode fail with the setting key and the file position,
// not a fragment the decoder created.
func parseRefSpan(doc []byte, start, end int, prefix []string) error {
	if start < 0 || end > len(doc) || start >= end {
		return nil
	}
	line, col, _ := fragmentAt(doc, start)
	scan := refScan{
		doc:      doc,
		key:      strings.Join(prefix, "."),
		fragLine: line,
		fragCol:  col,
		found:    true,
	}
	_, err := parseRefs(doc[start:end], scan)
	return err
}

// parseRefAssignment reads the value of one assignment as a reference body.
func parseRefAssignment(
	kv *unstable.Node,
	parser *unstable.Parser,
	doc []byte,
	prefix []string,
) error {
	start, end, ok := valueSpan(parser, kv, doc)
	if !ok {
		return nil
	}
	return parseRefSpan(doc, start, end, prefix)
}

// tableParts joins the enclosing table header with the assignment key
// into one document path. A dotted key extends the header the same way
// the decoder extends it.
func tableParts(table []string, expr *unstable.Node) []string {
	parts := make([]string, 0, len(table)+1)
	parts = append(parts, table...)
	keys := expr.Key()
	for keys.Next() {
		parts = append(parts, string(keys.Node().Data))
	}
	return parts
}

// secretTarget resolves a document path to the secret shape behind it. It
// follows the decoder. Names fold case, maps consume one part, an element
// index consumes one part, and an embedded struct without a name answers at
// the outer level.
//
// It returns two paths. The prefix is the dotted setting key a message
// names, and an element index never joins it. The settled path names each
// field or map entry the walk reached, spelled the way the type spells it,
// so a document key that differs in case still matches the same setting.
func secretTarget(t reflect.Type, parts []string) (reflect.Type, []string, []string, bool) {
	node := t
	prefix := make([]string, 0, len(parts))
	settled := make([]string, 0, len(parts))
	for _, part := range parts {
		next, name, at, ok := secretStep(node, part)
		if !ok {
			return nil, nil, nil, false
		}
		if name != "" {
			prefix = append(prefix, name)
		}
		settled = append(settled, at)
		node = next
	}
	for node.Kind() == reflect.Pointer {
		node = node.Elem()
	}
	if node == secretType || isSecretList(node) {
		return node, prefix, settled, true
	}
	return nil, nil, nil, false
}

// secretStep consumes one document key part. A struct answers with the
// field behind that part, a map answers with its entry shape, and a list
// answers with its element shape for an index part. The element is a
// struct, a map, a list, or a Secret, after pointers are followed. The
// field or entry type is kept whole, so a map of secrets still resolves
// after the entry name.
//
// It returns the message name for the part, which is empty for an element
// index, and the settled name, which is the field key, the entry name or
// the index the document wrote.
func secretStep(node reflect.Type, part string) (reflect.Type, string, string, bool) {
	for node.Kind() == reflect.Pointer {
		node = node.Elem()
	}
	switch node.Kind() {
	case reflect.Struct:
		if node == secretType {
			return nil, "", "", false
		}
		field, name, ok := secretField(node, part)
		if !ok {
			return nil, "", "", false
		}
		return field.Type, name, name, true
	case reflect.Map:
		return node.Elem(), part, part, true
	case reflect.Slice, reflect.Array:
		if _, err := strconv.Atoi(part); err != nil {
			return nil, "", "", false
		}
		elem := node.Elem()
		for elem.Kind() == reflect.Pointer {
			elem = elem.Elem()
		}
		switch elem.Kind() {
		case reflect.Struct, reflect.Map, reflect.Slice, reflect.Array:
			return elem, "", part, true
		}
		return nil, "", "", false
	default:
		return nil, "", "", false
	}
}

// isSecretList reports whether a settled node holds an ordered list of
// secret settings, either a slice or a fixed size array.
func isSecretList(node reflect.Type) bool {
	for node.Kind() == reflect.Pointer {
		node = node.Elem()
	}
	if node.Kind() != reflect.Slice && node.Kind() != reflect.Array {
		return false
	}
	elem := node.Elem()
	for elem.Kind() == reflect.Pointer {
		elem = elem.Elem()
	}
	return elem == secretType
}

// secretField finds the settings field behind one document key part. It
// matches the decoder, which folds case and flattens an embedded struct
// without a name. The name is the field key for a struct field and the
// entry name for a map entry.
func secretField(node reflect.Type, part string) (reflect.StructField, string, bool) {
	var folded reflect.StructField
	foldedName := ""
	found := false
	for i := 0; i < node.NumField(); i++ {
		field := node.Field(i)
		if field.Anonymous && embeddedFlat(field) {
			flat := field.Type
			for flat.Kind() == reflect.Pointer {
				flat = flat.Elem()
			}
			if flat.Kind() == reflect.Struct && flat != secretType {
				if sub, name, ok := secretField(flat, part); ok {
					return sub, name, true
				}
				continue
			}
		}
		name, ok := fieldKey(field)
		if !ok {
			continue
		}
		if name == part {
			return field, name, true
		}
		if !found && strings.EqualFold(name, part) {
			folded = field
			foldedName = name
			found = true
		}
	}
	if found {
		return folded, foldedName, true
	}
	return reflect.StructField{}, "", false
}

// checkRefInlineValue walks the children of one inline table that is not
// itself a secret. A child that is a secret is checked as a reference,
// whether it is an inline table or an inline array. A child that is a list
// of secrets is checked as a list of references. A child that is an inline
// table, an inline array of structs, or a nested array is walked again
// with the path extended by its key or element index. A nested array that
// holds a secret list is checked as that list.
func checkRefInlineValue(
	t reflect.Type,
	parts []string,
	value *unstable.Node,
	parser *unstable.Parser,
	doc []byte,
) error {
	children := value.Children()
	for children.Next() {
		child := children.Node()
		if child.Kind != unstable.KeyValue {
			continue
		}
		childParts := tableParts(parts, child)
		childValue := child.Value()
		target, prefix, _, ok := secretTarget(t, childParts)
		if ok && target == secretType {
			if childValue.Kind == unstable.InlineTable {
				if err := checkRefInlineTable(target, prefix, child, parser, doc); err != nil {
					return err
				}
			}
			if childValue.Kind == unstable.Array {
				if err := parseRefAssignment(child, parser, doc, prefix); err != nil {
					return err
				}
			}
			continue
		}
		if childValue.Kind == unstable.Array {
			if ok && isSecretList(target) {
				if err := checkRefArray(target, prefix, child, parser, doc); err != nil {
					return err
				}
				continue
			}
			if err := checkRefInlineArray(t, childParts, childValue, parser, doc); err != nil {
				return err
			}
			continue
		}
		if childValue.Kind == unstable.InlineTable {
			if err := checkRefInlineValue(t, childParts, childValue, parser, doc); err != nil {
				return err
			}
		}
	}
	return nil
}

// checkRefInlineArray walks an array that is not itself a secret list.
// An array element that is a secret list is checked as a list of
// references. An inline table element is walked as a struct or a map.
// Any other array element is walked again with its index on the path.
// The index never joins a setting-key message.
func checkRefInlineArray(
	t reflect.Type,
	parts []string,
	value *unstable.Node,
	parser *unstable.Parser,
	doc []byte,
) error {
	children := value.Children()
	i := 0
	for children.Next() {
		elem := children.Node()
		if elem.Kind == unstable.Comment {
			continue
		}
		elemParts := appendPath(parts, strconv.Itoa(i))
		switch elem.Kind {
		case unstable.InlineTable:
			if err := checkRefInlineValue(t, elemParts, elem, parser, doc); err != nil {
				return err
			}
		case unstable.Array:
			target, prefix, _, ok := secretTarget(t, elemParts)
			if ok && isSecretList(target) {
				if err := checkNestedSecretArray(prefix, elem, parser, doc); err != nil {
					return err
				}
				break
			}
			if err := checkRefInlineArray(t, elemParts, elem, parser, doc); err != nil {
				return err
			}
		default:
			continue
		}
		i++
	}
	return nil
}

// nestedArraySpan returns the document span of an array node whose parser
// range is empty. go-toml leaves Array.Raw unset, so the span is rebuilt
// from the first child's offset back to '[' and a matching close.
func nestedArraySpan(elem *unstable.Node, doc []byte) (int, int, bool) {
	if elem.Raw.Length > 0 {
		start := int(elem.Raw.Offset)
		end := start + int(elem.Raw.Length)
		if start >= 0 && end <= len(doc) && start < end {
			return start, end, true
		}
	}
	children := elem.Children()
	var first *unstable.Node
	for children.Next() {
		n := children.Node()
		if n.Kind == unstable.Comment {
			continue
		}
		first = n
		break
	}
	if first == nil || first.Raw.Length == 0 {
		return 0, 0, false
	}
	start := int(first.Raw.Offset)
	for start > 0 && doc[start] != '[' {
		start--
	}
	if start >= len(doc) || doc[start] != '[' {
		return 0, 0, false
	}
	end := matchListEnd(doc, start)
	if end < 0 {
		return 0, 0, false
	}
	return start, end, true
}

// matchListEnd returns the index after the ']' that closes the list at
// start. Brackets inside a quoted string do not change the depth.
func matchListEnd(doc []byte, start int) int {
	depth := 0
	var quote byte
	for i := start; i < len(doc); i++ {
		c := doc[i]
		switch {
		case quote == '"':
			if c == '\\' {
				i++
				continue
			}
			if c == '"' {
				quote = 0
			}
		case quote == '\'':
			if c == '\'' {
				quote = 0
			}
		case c == '"' || c == '\'':
			quote = c
		case c == '[':
			depth++
		case c == ']':
			depth--
			if depth == 0 {
				return i + 1
			}
		}
	}
	return -1
}

// checkNestedSecretArray checks locator keys of a secret list written as
// an array element, then reads the list body so a typed fault and an
// unknown read mode name the setting.
func checkNestedSecretArray(prefix []string, elem *unstable.Node, parser *unstable.Parser, doc []byte) error {
	inner := elem.Children()
	for inner.Next() {
		it := inner.Node()
		if it.Kind == unstable.Comment {
			continue
		}
		if it.Kind != unstable.InlineTable {
			continue
		}
		tables := it.Children()
		for tables.Next() {
			child := tables.Node()
			name, row, col := keyNameAt(child, parser, doc)
			if !isRefDocKey(name) {
				return fmt.Errorf(
					"%w: %sunknown key %q at line %d, column %d",
					ErrInvalidRef,
					secretPath(prefix),
					name,
					row,
					col,
				)
			}
		}
	}
	start, end, ok := nestedArraySpan(elem, doc)
	if !ok {
		return nil
	}
	return parseRefSpan(doc, start, end, prefix)
}

// checkRefInlineTable validates one inline table reference. An unknown
// locator key, a locator of the wrong type, and an unknown read mode fail
// with the setting key and the position in the file.
func checkRefInlineTable(
	target reflect.Type,
	prefix []string,
	kv *unstable.Node,
	parser *unstable.Parser,
	doc []byte,
) error {
	if target != secretType {
		return nil
	}
	value := kv.Value()
	children := value.Children()
	for children.Next() {
		child := children.Node()
		name, row, col := keyNameAt(child, parser, doc)
		if !isRefDocKey(name) {
			return fmt.Errorf(
				"%w: %sunknown key %q at line %d, column %d",
				ErrInvalidRef,
				secretPath(prefix),
				name,
				row,
				col,
			)
		}
	}
	return parseRefAssignment(kv, parser, doc, prefix)
}

// checkRefArray validates the locator keys of each inline table in one
// inline array. Each element is one ordered reference, so an unknown key
// fails with the setting key and the position in the file. The array body
// is then read, so a typed fault and an unknown read mode fail the same way.
func checkRefArray(
	target reflect.Type,
	prefix []string,
	kv *unstable.Node,
	parser *unstable.Parser,
	doc []byte,
) error {
	if !isSecretList(target) {
		return nil
	}
	value := kv.Value()
	children := value.Children()
	for children.Next() {
		elem := children.Node()
		if elem.Kind == unstable.Comment {
			continue
		}
		if elem.Kind != unstable.InlineTable {
			continue
		}
		tables := elem.Children()
		for tables.Next() {
			child := tables.Node()
			name, row, col := keyNameAt(child, parser, doc)
			if !isRefDocKey(name) {
				return fmt.Errorf(
					"%w: %sunknown key %q at line %d, column %d",
					ErrInvalidRef,
					secretPath(prefix),
					name,
					row,
					col,
				)
			}
		}
	}
	return parseRefAssignment(kv, parser, doc, prefix)
}

// keyNameAt reads one key and its position in the file. The parser reports
// the byte offset of every key, so a key that spells the same name as an
// earlier key or as a comment still reports its own line and column.
func keyNameAt(kv *unstable.Node, parser *unstable.Parser, doc []byte) (string, int, int) {
	keys := kv.Key()
	name := ""
	raw := []byte(nil)
	at := -1
	for keys.Next() {
		node := keys.Node()
		name = string(node.Data)
		raw = parser.Raw(node.Raw)
		at = int(node.Raw.Offset)
	}
	return keyAt(name, raw, at, doc)
}

// keyAt converts a key's raw bytes and its offset into the spelling and the
// position in the file. The spelling is read back from the document, so the
// message names the case the file wrote. It falls back to the key name at
// the first line when the offset lies outside the document.
func keyAt(name string, raw []byte, at int, doc []byte) (string, int, int) {
	if len(raw) == 0 || at < 0 || at+len(raw) > len(doc) {
		return name, 1, 1
	}
	line, col, _ := fragmentAt(doc, at)
	return string(doc[at : at+len(raw)]), line, col
}

// checkSecretList checks the document node of a field holding a list of
// secrets. An inline array or an array of tables holds one reference table
// per element, so a literal element is refused by key. A single table is
// not a list, and the decoder hands each of its values to one element, so
// the shape is refused by key rather than misread.
func checkSecretList(node any, prefix []string, reg *Registry) error {
	key := strings.Join(prefix, ".")
	switch v := node.(type) {
	case map[string]any:
		if err := checkRefSource(v, key, reg); err != nil {
			return err
		}
		return fmt.Errorf(
			"%w: %s: a list setting takes an array of tables or an inline array, not one table",
			ErrInvalidRef,
			key,
		)
	case []any:
		for _, item := range v {
			ref, ok := item.(map[string]any)
			if !ok {
				return fmt.Errorf("%w: %s", ErrInlineSecret, key)
			}
			if err := checkRefSource(ref, key, reg); err != nil {
				return err
			}
		}
		return nil
	default:
		return fmt.Errorf("%w: %s", ErrInlineSecret, key)
	}
}

// checkSecretNode refuses a literal where a reference belongs, then checks
// every reference's source name.
func checkSecretNode(node any, path []string, reg *Registry) error {
	key := strings.Join(path, ".")
	switch v := node.(type) {
	case map[string]any:
		return checkRefSource(v, key, reg)
	case []any:
		if len(v) == 0 {
			return nil
		}
		for _, item := range v {
			ref, ok := item.(map[string]any)
			if !ok {
				return fmt.Errorf("%w: %s", ErrInlineSecret, key)
			}
			if err := checkRefSource(ref, key, reg); err != nil {
				return err
			}
		}
		return nil
	default:
		return fmt.Errorf("%w: %s", ErrInlineSecret, key)
	}
}

// embeddedFlat reports whether a field is an embedded struct the decoder
// flattens, so its fields answer at the keys of the outer struct. A toml tag
// that names the field keeps it a nested table, and a skipped field never
// appears.
func embeddedFlat(field reflect.StructField) bool {
	return strings.Split(field.Tag.Get("toml"), ",")[0] == ""
}

// checkRefSource refuses a reference whose source name is missing or is not
// registered.
func checkRefSource(ref map[string]any, key string, reg *Registry) error {
	raw, ok := lookupFold(ref, "source")
	if !ok {
		return fmt.Errorf("%w: %s: missing source", ErrInvalidRef, key)
	}
	name, ok := raw.(string)
	if !ok || strings.TrimSpace(name) == "" {
		return fmt.Errorf("%w: %s: source must be a name", ErrInvalidRef, key)
	}
	name = strings.TrimSpace(name)
	if _, ok := reg.Lookup(name); !ok {
		return fmt.Errorf("%w: %s: %q", ErrUnknownSource, key, name)
	}
	return nil
}

// fieldKey returns the document key a struct field answers to, or false when
// the field is not part of the document.
func fieldKey(field reflect.StructField) (string, bool) {
	tag := field.Tag.Get("toml")
	if tag == "-" {
		return "", false
	}
	if name := strings.Split(tag, ",")[0]; name != "" {
		return name, true
	}
	if field.PkgPath != "" {
		return "", false
	}
	return field.Name, true
}

// lookupFold finds a key in a parsed table, matching without case, which is
// how the decoder matches a field name.
func lookupFold(tree map[string]any, key string) (any, bool) {
	if v, ok := tree[key]; ok {
		return v, true
	}
	for k, v := range tree {
		if strings.EqualFold(k, key) {
			return v, true
		}
	}
	return nil, false
}

// sortedKeys returns a table's keys in sorted order, so a failure on an
// ordered list is deterministic.
func sortedKeys(tree map[string]any) []string {
	keys := make([]string, 0, len(tree))
	for k := range tree {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// appendPath returns prefix with key appended, without sharing the backing
// array of prefix.
func appendPath(prefix []string, key string) []string {
	out := make([]string, len(prefix)+1)
	copy(out, prefix)
	out[len(prefix)] = key
	return out
}

// decodeError turns a parser failure into a message that names every unknown
// key and its position, or the malformed value.
func decodeError(err error) error {
	var strict *toml.StrictMissingError
	if errors.As(err, &strict) {
		var parts []string
		for _, e := range strict.Errors {
			row, col := e.Position()
			parts = append(parts, fmt.Sprintf("%q at line %d, column %d", strings.Join(e.Key(), "."), row, col))
		}
		return fmt.Errorf("%w: %s", ErrUnknownKey, strings.Join(parts, ", "))
	}
	var de *toml.DecodeError
	if errors.As(err, &de) {
		return fmt.Errorf("%w: %s", ErrMalformed, strings.TrimSpace(de.String()))
	}
	return err
}

// refDocType is the strict shape each Secret position takes in the mirror
// document. Its unknown key errors carry the offender and its position.
var refDocType = reflect.TypeOf(refDoc{})

// isRefDocKey reports whether name is a locator key of the strict
// reference shape, matching the decoder without regard to case.
func isRefDocKey(name string) bool {
	for i := 0; i < refDocType.NumField(); i++ {
		tag := strings.Split(refDocType.Field(i).Tag.Get("toml"), ",")[0]
		if strings.EqualFold(tag, name) {
			return true
		}
	}
	return false
}

// secretPath renders the dotted key of a secret setting in the file. It
// appends the trailing colon the message format carries after the key.
func secretPath(trimmed []string) string {
	if len(trimmed) == 0 {
		return ""
	}
	return strings.Join(trimmed, ".") + ": "
}
