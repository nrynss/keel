package config

import (
	"context"
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"
)

// origin records which layer won for one non-secret setting.
type origin struct {
	source  string
	locator string
}

// secretState records how one secret resolved.
type secretState struct {
	source   string
	locator  string
	resolved bool
}

// collectKeys records every setting key as a default, and notes secret
// keys so a flag cannot set them.
func collectKeys(
	v reflect.Value,
	t reflect.Type,
	prefix []string,
	parent reflect.StructField,
	origins map[string]origin,
	secretKeys map[string]bool,
) error {
	return walkSettings(v, t, prefix, parent, func(s setting) error {
		if s.kind == settingSecret {
			secretKeys[s.key] = true
			return nil
		}
		if _, ok := origins[s.key]; !ok {
			origins[s.key] = origin{source: sourceDefault}
		}
		return nil
	})
}

// markFileOrigins records every non-secret key the document set, so the
// plan can name the file as the source until env or a flag replaces it.
func markFileOrigins(t reflect.Type, node any, prefix []string, filePath string, origins map[string]origin) {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t == secretType || isSecretList(t) {
		return
	}
	loc := "path=" + filePath
	switch t.Kind() {
	case reflect.Struct:
		tree, ok := node.(map[string]any)
		if !ok {
			return
		}
		for i := 0; i < t.NumField(); i++ {
			field := t.Field(i)
			if field.Anonymous && embeddedFlat(field) {
				markFileOrigins(field.Type, node, prefix, filePath, origins)
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
			ft := field.Type
			for ft.Kind() == reflect.Pointer {
				ft = ft.Elem()
			}
			if isScalarType(ft) || isScalarList(ft) || isScalarMap(ft) {
				origins[joinKey(appendPath(prefix, key))] = origin{source: sourceFile, locator: loc}
				continue
			}
			markFileOrigins(field.Type, child, appendPath(prefix, key), filePath, origins)
		}
	case reflect.Slice, reflect.Array:
		if isContainerElem(t.Elem()) {
			list, ok := node.([]any)
			if !ok {
				return
			}
			for i, item := range list {
				markFileOrigins(t.Elem(), item, appendPath(prefix, strconv.Itoa(i)), filePath, origins)
			}
		}
	case reflect.Map:
		tree, ok := node.(map[string]any)
		if !ok {
			return
		}
		if isContainerElem(t.Elem()) {
			for _, key := range sortedKeys(tree) {
				markFileOrigins(t.Elem(), tree[key], appendPath(prefix, key), filePath, origins)
			}
			return
		}
		if isScalarType(t.Elem()) {
			origins[joinKey(prefix)] = origin{source: sourceFile, locator: loc}
		}
	}
}

// applyEnv overlays non-secret scalars from the injected lookup.
func applyEnv(
	v reflect.Value,
	t reflect.Type,
	prefix []string,
	lookup func(string) (string, bool),
	origins map[string]origin,
) error {
	return walkSettings(v, t, prefix, reflect.StructField{}, func(s setting) error {
		if s.kind != settingScalar {
			return nil
		}
		envKey := overrideKey(s.key)
		val, ok := lookup(envKey)
		if !ok {
			return nil
		}
		if !s.val.IsValid() {
			return fmt.Errorf("config: cannot set %s: missing parent", s.key)
		}
		if err := setFromString(s.val, val); err != nil {
			return fmt.Errorf("config: cannot set %s: %w", s.key, err)
		}
		origins[s.key] = origin{source: sourceEnv, locator: "var=" + envKey}
		return nil
	})
}

// applyFlags overlays non-secret scalars from the injected flag map.
func applyFlags(
	v reflect.Value,
	t reflect.Type,
	prefix []string,
	flags map[string]string,
	origins map[string]origin,
	secretKeys map[string]bool,
) error {
	if len(flags) == 0 {
		return nil
	}
	used := make(map[string]bool, len(flags))
	err := walkSettings(v, t, prefix, reflect.StructField{}, func(s setting) error {
		if s.kind != settingScalar {
			return nil
		}
		key, val, ok := flagValue(flags, s.key)
		if !ok {
			return nil
		}
		if !s.val.IsValid() {
			return fmt.Errorf("config: cannot set %s: missing parent", s.key)
		}
		if err := setFromString(s.val, val); err != nil {
			return fmt.Errorf("config: cannot set %s: %w", s.key, err)
		}
		used[key] = true
		origins[s.key] = origin{source: sourceFlag, locator: "flag=" + key}
		return nil
	})
	if err != nil {
		return err
	}
	var unknown []string
	for k := range flags {
		if k == "" || used[k] {
			continue
		}
		unknown = append(unknown, k)
	}
	sort.Strings(unknown)
	if len(unknown) == 0 {
		return nil
	}
	if secretFlag(unknown[0], secretKeys) {
		return fmt.Errorf("config: flag %s names a secret setting", unknown[0])
	}
	return fmt.Errorf("config: unknown flag %s", strings.Join(unknown, ", "))
}

// flagValue finds a flag for one field path. The path wins over the
// derived override key when both are present.
func flagValue(flags map[string]string, path string) (string, string, bool) {
	if val, ok := flags[path]; ok {
		return path, val, true
	}
	envKey := overrideKey(path)
	if val, ok := flags[envKey]; ok {
		return envKey, val, true
	}
	return "", "", false
}

// secretFlag reports whether a flag key names a secret setting.
func secretFlag(key string, secretKeys map[string]bool) bool {
	if secretKeys[key] {
		return true
	}
	for path := range secretKeys {
		if overrideKey(path) == key {
			return true
		}
	}
	return false
}

// resolveSecrets tries each secret's references in order and installs a
// Reveal that returns the first value that succeeds. A secret with no
// references stays unresolved. A secret whose references all fail stops
// the load.
func resolveSecrets(
	ctx context.Context,
	v reflect.Value,
	t reflect.Type,
	prefix []string,
	parent reflect.StructField,
	reg *Registry,
) (map[string]secretState, error) {
	out := make(map[string]secretState)
	err := walkSettings(v, t, prefix, parent, func(s setting) error {
		if s.kind != settingSecret {
			return nil
		}
		sec, ok := secretFrom(s.val)
		if !ok || len(sec.refs) == 0 {
			out[s.key] = secretState{source: sourceNone}
			return nil
		}
		var last error
		var lastRef Ref
		for _, ref := range sec.refs {
			src, ok := reg.Lookup(ref.Source())
			if !ok {
				last = fmt.Errorf("%w: %q", ErrUnknownSource, ref.Source())
				lastRef = ref
				continue
			}
			val, err := src.Resolve(ctx, ref)
			if err != nil {
				last = err
				lastRef = ref
				continue
			}
			sec.bind(val)
			if err := setSecret(s.val, sec); err != nil {
				return err
			}
			out[s.key] = secretState{source: ref.Source(), locator: ref.locator(), resolved: true}
			return nil
		}
		loc := lastRef.locator()
		if loc == "" {
			loc = sourceNone
		}
		srcName := lastRef.Source()
		if srcName == "" {
			srcName = sourceNone
		}
		out[s.key] = secretState{source: srcName, locator: loc}
		if last == nil {
			last = ErrUnresolved
		}
		return fmt.Errorf("%w: %s (source %s locator %s): %w", ErrResolve, s.key, srcName, loc, last)
	})
	return out, err
}

// checkRequired fails when a required field is still zero, or a required
// secret is still unresolved. The message names the key, source, and
// locator.
func checkRequired(
	v reflect.Value,
	t reflect.Type,
	prefix []string,
	parent reflect.StructField,
	origins map[string]origin,
	secrets map[string]secretState,
) error {
	return walkSettings(v, t, prefix, parent, func(s setting) error {
		if !s.required {
			return nil
		}
		if s.kind == settingSecret {
			st := secrets[s.key]
			if st.resolved {
				return nil
			}
			src, loc := st.source, st.locator
			if src == "" {
				src = sourceNone
			}
			if loc == "" {
				loc = sourceNone
			}
			return fmt.Errorf("%w: %s (source %s locator %s)", ErrRequired, s.key, src, loc)
		}
		if s.val.IsValid() && !s.val.IsZero() {
			return nil
		}
		o := origins[s.key]
		src, loc := o.source, o.locator
		if src == "" {
			src = sourceNone
		}
		if loc == "" {
			loc = sourceNone
		}
		return fmt.Errorf("%w: %s (source %s locator %s)", ErrRequired, s.key, src, loc)
	})
}

// secretFrom reads a Secret from a value or a pointer to one.
func secretFrom(v reflect.Value) (Secret, bool) {
	if !v.IsValid() {
		return Secret{}, false
	}
	if v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return Secret{}, false
		}
		v = v.Elem()
	}
	if v.Type() != secretType || !v.CanInterface() {
		return Secret{}, false
	}
	return v.Interface().(Secret), true
}

// setSecret writes s back onto v, which may be a Secret or a *Secret.
func setSecret(v reflect.Value, s Secret) error {
	if !v.IsValid() {
		return fmt.Errorf("config: cannot set secret")
	}
	if v.Kind() == reflect.Pointer {
		if v.IsNil() {
			if !v.CanSet() {
				return fmt.Errorf("config: cannot set secret")
			}
			held := reflect.New(secretType)
			held.Elem().Set(reflect.ValueOf(s))
			v.Set(held)
			return nil
		}
		v = v.Elem()
	}
	if !v.CanSet() {
		return fmt.Errorf("config: cannot set secret")
	}
	v.Set(reflect.ValueOf(s))
	return nil
}

// overrideKey derives the environment override key from a field path.
// render.max_seconds becomes RENDER_MAX_SECONDS. Dots and hyphens become
// underscores. There is no application prefix.
func overrideKey(path string) string {
	s := strings.ReplaceAll(path, ".", "_")
	s = strings.ReplaceAll(s, "-", "_")
	return strings.ToUpper(s)
}

// isRequired reports whether a field carries config:"required".
func isRequired(field reflect.StructField) bool {
	for _, part := range strings.Split(field.Tag.Get("config"), ",") {
		if strings.TrimSpace(part) == "required" {
			return true
		}
	}
	return false
}

// settingKind names what a walk visits.
type settingKind int

const (
	settingScalar settingKind = iota
	settingSecret
	settingList
)

// setting is one plan-able node the walk visits.
type setting struct {
	key      string
	val      reflect.Value
	kind     settingKind
	required bool
}

type settingFn func(setting) error

// walkSettings visits every setting reachable from v. Nested structs, maps,
// and lists are walked. A nil pointer to a nested struct is allocated when
// it can be set. A scalar slice or map is one setting. Each secret in a
// list or map is its own setting. Only a Secret, or a list or map of
// Secret as the leaf, is a secret setting.
func walkSettings(v reflect.Value, t reflect.Type, prefix []string, parent reflect.StructField, fn settingFn) error {
	for t.Kind() == reflect.Pointer {
		elem := t.Elem()
		if elem == secretType || isScalarType(elem) {
			break
		}
		t = elem
		if v.IsValid() && v.Kind() == reflect.Pointer {
			if v.IsNil() {
				if v.CanSet() {
					v.Set(reflect.New(t))
					v = v.Elem()
				} else {
					v = reflect.Value{}
				}
			} else {
				v = v.Elem()
			}
		} else {
			v = reflect.Value{}
		}
	}
	if t.Kind() == reflect.Pointer {
		elem := t.Elem()
		if elem == secretType {
			return fn(setting{
				key:      joinKey(prefix),
				val:      v,
				kind:     settingSecret,
				required: isRequired(parent),
			})
		}
		return fn(setting{
			key:      joinKey(prefix),
			val:      v,
			kind:     settingScalar,
			required: isRequired(parent),
		})
	}
	if t == secretType {
		return fn(setting{
			key:      joinKey(prefix),
			val:      v,
			kind:     settingSecret,
			required: isRequired(parent),
		})
	}
	switch t.Kind() {
	case reflect.Struct:
		return walkStructSettings(v, t, prefix, fn)
	case reflect.Slice, reflect.Array:
		if isSecretList(t) {
			return walkSecretList(v, prefix, parent, fn)
		}
		if isContainerElem(t.Elem()) {
			return walkIndexed(v, t.Elem(), prefix, fn)
		}
		return fn(setting{
			key:      joinKey(prefix),
			val:      v,
			kind:     settingList,
			required: isRequired(parent),
		})
	case reflect.Map:
		if isSecretElem(t.Elem()) {
			return walkSecretMap(v, prefix, parent, fn)
		}
		if isContainerElem(t.Elem()) {
			return walkStructMap(v, t.Elem(), prefix, fn)
		}
		return fn(setting{
			key:      joinKey(prefix),
			val:      v,
			kind:     settingList,
			required: isRequired(parent),
		})
	default:
		return fn(setting{
			key:      joinKey(prefix),
			val:      v,
			kind:     settingScalar,
			required: isRequired(parent),
		})
	}
}

// walkStructSettings visits each exported field of a struct, flattening
// an embedded struct that has no toml name.
func walkStructSettings(v reflect.Value, t reflect.Type, prefix []string, fn settingFn) error {
	for i := 0; i < t.NumField(); i++ {
		field := t.Field(i)
		var fv reflect.Value
		if v.IsValid() && v.Kind() == reflect.Struct {
			fv = v.Field(i)
		}
		if field.Anonymous && embeddedFlat(field) {
			if err := walkSettings(fv, field.Type, prefix, field, fn); err != nil {
				return err
			}
			continue
		}
		name, ok := fieldKey(field)
		if !ok {
			continue
		}
		if err := walkSettings(fv, field.Type, appendPath(prefix, name), field, fn); err != nil {
			return err
		}
	}
	return nil
}

// walkSecretList visits each secret in a list. An empty list is one
// unresolved secret at the list key, so a required list can fail.
func walkSecretList(v reflect.Value, prefix []string, parent reflect.StructField, fn settingFn) error {
	if !v.IsValid() || v.Len() == 0 {
		return fn(setting{
			key:      joinKey(prefix),
			kind:     settingSecret,
			required: isRequired(parent),
		})
	}
	for i := 0; i < v.Len(); i++ {
		if err := walkSettings(v.Index(i), v.Index(i).Type(), appendPath(prefix, strconv.Itoa(i)), parent, fn); err != nil {
			return err
		}
	}
	return nil
}

// walkIndexed visits each element of a list of structs.
func walkIndexed(v reflect.Value, elem reflect.Type, prefix []string, fn settingFn) error {
	if !v.IsValid() {
		return nil
	}
	for i := 0; i < v.Len(); i++ {
		if err := walkSettings(v.Index(i), elem, appendPath(prefix, strconv.Itoa(i)), reflect.StructField{}, fn); err != nil {
			return err
		}
	}
	return nil
}

// walkSecretMap visits each secret map entry. MapIndex returns a copy, so
// the walk copies it into an addressable value and writes it back.
func walkSecretMap(v reflect.Value, prefix []string, parent reflect.StructField, fn settingFn) error {
	if !v.IsValid() || v.Len() == 0 {
		return fn(setting{
			key:      joinKey(prefix),
			kind:     settingSecret,
			required: isRequired(parent),
		})
	}
	keys := v.MapKeys()
	sort.Slice(keys, func(i, j int) bool { return keys[i].String() < keys[j].String() })
	for _, k := range keys {
		item := v.MapIndex(k)
		if !item.IsValid() {
			continue
		}
		elem := item.Type()
		held := reflect.New(elem).Elem()
		held.Set(item)
		if err := walkSettings(held, elem, appendPath(prefix, k.String()), parent, fn); err != nil {
			return err
		}
		v.SetMapIndex(k, held)
	}
	return nil
}

// walkStructMap visits each struct map entry. MapIndex returns a copy, so
// the walk copies it into an addressable value and writes it back.
func walkStructMap(v reflect.Value, elem reflect.Type, prefix []string, fn settingFn) error {
	if !v.IsValid() {
		return nil
	}
	keys := v.MapKeys()
	sort.Slice(keys, func(i, j int) bool { return keys[i].String() < keys[j].String() })
	for _, k := range keys {
		item := v.MapIndex(k)
		if !item.IsValid() {
			continue
		}
		held := reflect.New(elem).Elem()
		held.Set(item)
		if err := walkSettings(held, elem, appendPath(prefix, k.String()), reflect.StructField{}, fn); err != nil {
			return err
		}
		v.SetMapIndex(k, held)
	}
	return nil
}

// setFromString writes s into v, allocating a nil pointer when needed.
func setFromString(v reflect.Value, s string) error {
	if !v.IsValid() {
		return fmt.Errorf("invalid destination")
	}
	if v.Kind() == reflect.Pointer {
		if v.IsNil() {
			if !v.CanSet() {
				return fmt.Errorf("cannot set")
			}
			v.Set(reflect.New(v.Type().Elem()))
		}
		return setFromString(v.Elem(), s)
	}
	if !v.CanSet() {
		return fmt.Errorf("cannot set")
	}
	switch v.Kind() {
	case reflect.String:
		v.SetString(s)
		return nil
	case reflect.Bool:
		b, err := strconv.ParseBool(s)
		if err != nil {
			return err
		}
		v.SetBool(b)
		return nil
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		n, err := strconv.ParseInt(s, 10, v.Type().Bits())
		if err != nil {
			return err
		}
		v.SetInt(n)
		return nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		n, err := strconv.ParseUint(s, 10, v.Type().Bits())
		if err != nil {
			return err
		}
		v.SetUint(n)
		return nil
	case reflect.Float32, reflect.Float64:
		n, err := strconv.ParseFloat(s, v.Type().Bits())
		if err != nil {
			return err
		}
		v.SetFloat(n)
		return nil
	default:
		return fmt.Errorf("unsupported type %s", v.Type())
	}
}

// isScalarType reports a bool, string, or numeric type after pointers.
func isScalarType(t reflect.Type) bool {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t == secretType {
		return false
	}
	switch t.Kind() {
	case reflect.Bool, reflect.String,
		reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64,
		reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64,
		reflect.Float32, reflect.Float64:
		return true
	default:
		return false
	}
}

// isScalarList reports a slice or array of scalars.
func isScalarList(t reflect.Type) bool {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Kind() != reflect.Slice && t.Kind() != reflect.Array {
		return false
	}
	return isScalarType(t.Elem())
}

// isScalarMap reports a map with scalar values.
func isScalarMap(t reflect.Type) bool {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t.Kind() != reflect.Map {
		return false
	}
	return isScalarType(t.Elem())
}

// isStructElem reports a struct that is not a Secret, after pointers.
func isStructElem(t reflect.Type) bool {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t.Kind() == reflect.Struct && t != secretType
}

// isContainerElem reports a map, list, or struct that is not a Secret,
// after pointers. Those elements are walked. A scalar element is one
// settingList.
func isContainerElem(t reflect.Type) bool {
	if isStructElem(t) {
		return true
	}
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch t.Kind() {
	case reflect.Map, reflect.Slice, reflect.Array:
		return true
	default:
		return false
	}
}

// isSecretElem reports a Secret, after pointers.
func isSecretElem(t reflect.Type) bool {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	return t == secretType
}

// joinKey joins a dotted path. An empty path is the empty string.
func joinKey(prefix []string) string {
	return strings.Join(prefix, ".")
}
