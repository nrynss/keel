package config

import (
	"fmt"
	"reflect"
	"sort"
	"strconv"
	"strings"

	"github.com/pelletier/go-toml/v2"
)

// Plan records how each setting resolved. String prints one line per key.
// No line carries a value.
type Plan struct {
	lines []planLine
}

// planLine is one row of the resolution plan.
type planLine struct {
	key      string
	source   string
	locator  string
	resolved bool
}

const (
	statusResolved   = "resolved"
	statusUnresolved = "unresolved"
)

// String prints one tab separated line per key: name, source, locator, and
// whether it resolved. Lines are sorted by key. A trailing newline follows
// a non-empty plan.
func (p Plan) String() string {
	if len(p.lines) == 0 {
		return ""
	}
	var b strings.Builder
	for _, line := range p.lines {
		b.WriteString(line.format())
		b.WriteByte('\n')
	}
	return b.String()
}

// format renders one plan line. Tabs separate the four fields so a locator
// may contain spaces.
func (l planLine) format() string {
	status := statusUnresolved
	if l.resolved {
		status = statusResolved
	}
	return l.key + "\t" + l.source + "\t" + l.locator + "\t" + status
}

// Dump writes the effective settings as TOML. Every secret field is
// replaced by its plan line, never by a value.
func Dump(dst any, plan Plan) ([]byte, error) {
	el, err := destStruct(dst)
	if err != nil {
		return nil, err
	}
	idx := make(map[string]planLine, len(plan.lines))
	for _, line := range plan.lines {
		idx[line.key] = line
	}
	tree, err := dumpValue(el, nil, idx)
	if err != nil {
		return nil, err
	}
	if tree == nil {
		tree = map[string]any{}
	}
	return toml.Marshal(tree)
}

// buildPlan walks the loaded settings and emits one sorted line per key.
func buildPlan(
	v reflect.Value,
	t reflect.Type,
	prefix []string,
	parent reflect.StructField,
	origins map[string]origin,
	secrets map[string]secretState,
) Plan {
	var lines []planLine
	_ = walkSettings(v, t, prefix, parent, func(s setting) error {
		if s.kind == settingSecret {
			st := secrets[s.key]
			src := st.source
			if src == "" {
				src = sourceNone
			}
			lines = append(lines, planLine{
				key:      s.key,
				source:   src,
				locator:  st.locator,
				resolved: st.resolved,
			})
			return nil
		}
		o := origins[s.key]
		src := o.source
		if src == "" {
			src = sourceDefault
		}
		lines = append(lines, planLine{
			key:      s.key,
			source:   src,
			locator:  o.locator,
			resolved: true,
		})
		return nil
	})
	sort.Slice(lines, func(i, j int) bool { return lines[i].key < lines[j].key })
	return Plan{lines: lines}
}

// dumpValue turns one value into a TOML tree, replacing secrets with plan
// lines.
func dumpValue(v reflect.Value, prefix []string, idx map[string]planLine) (any, error) {
	if !v.IsValid() {
		return nil, nil
	}
	for v.Kind() == reflect.Pointer {
		if v.IsNil() {
			return nil, nil
		}
		v = v.Elem()
	}
	if v.Type() == secretType {
		return dumpSecretLine(joinKey(prefix), idx), nil
	}
	switch v.Kind() {
	case reflect.Struct:
		return dumpStruct(v, prefix, idx)
	case reflect.Slice, reflect.Array:
		return dumpList(v, prefix, idx)
	case reflect.Map:
		return dumpMap(v, prefix, idx)
	default:
		if v.CanInterface() {
			return v.Interface(), nil
		}
		return nil, nil
	}
}

// dumpSecretLine returns the plan line for a secret key.
func dumpSecretLine(key string, idx map[string]planLine) string {
	if line, ok := idx[key]; ok {
		return line.format()
	}
	return planLine{key: key, source: sourceNone}.format()
}

// dumpStruct writes a table, flattening an embedded struct with no name.
func dumpStruct(v reflect.Value, prefix []string, idx map[string]planLine) (any, error) {
	out := make(map[string]any)
	t := v.Type()
	for i := 0; i < t.NumField(); i++ {
		field := t.Field(i)
		fv := v.Field(i)
		if field.Anonymous && embeddedFlat(field) {
			nested, err := dumpValue(fv, prefix, idx)
			if err != nil {
				return nil, err
			}
			table, ok := nested.(map[string]any)
			if !ok {
				continue
			}
			for k, val := range table {
				out[k] = val
			}
			continue
		}
		name, ok := fieldKey(field)
		if !ok {
			continue
		}
		child, err := dumpValue(fv, appendPath(prefix, name), idx)
		if err != nil {
			return nil, err
		}
		if child == nil {
			continue
		}
		out[name] = child
	}
	return out, nil
}

// dumpList writes an array. A secret list writes plan lines.
func dumpList(v reflect.Value, prefix []string, idx map[string]planLine) (any, error) {
	n := v.Len()
	out := make([]any, n)
	for i := 0; i < n; i++ {
		child, err := dumpValue(v.Index(i), appendPath(prefix, strconv.Itoa(i)), idx)
		if err != nil {
			return nil, err
		}
		out[i] = child
	}
	return out, nil
}

// dumpMap writes a table of entries. A secret map writes plan lines.
func dumpMap(v reflect.Value, prefix []string, idx map[string]planLine) (any, error) {
	out := make(map[string]any, v.Len())
	keys := v.MapKeys()
	sort.Slice(keys, func(i, j int) bool { return keys[i].String() < keys[j].String() })
	for _, k := range keys {
		name := fmt.Sprint(k.Interface())
		child, err := dumpValue(v.MapIndex(k), appendPath(prefix, name), idx)
		if err != nil {
			return nil, err
		}
		if child == nil {
			continue
		}
		out[name] = child
	}
	return out, nil
}
