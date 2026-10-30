package yaml

import (
	"errors"
	"fmt"
	"math"
	"reflect"
	"strconv"
	"strings"
)

// Unmarshaler decodes itself from a node. Errors it returns, other than an *Error, are
// given the node's position and path.
type Unmarshaler interface {
	UnmarshalYAML(n *Node) error
}

// Decode stores a node in v, a pointer to a struct, map, slice or scalar. Struct fields
// are matched by their `yaml:"name"` tag; fields without one, or tagged "-", are never
// set, and a key that matches no field is an error. Integers, floats and booleans must be
// plain scalars; strings may be any scalar. Null leaves a value at its zero.
func Decode(n *Node, v any) error {
	rv := reflect.ValueOf(v)
	if rv.Kind() != reflect.Pointer || rv.IsNil() {
		return errors.New("yaml: Decode needs a non-nil pointer")
	}
	return decode(n, rv.Elem(), "")
}

var unmarshalerType = reflect.TypeFor[Unmarshaler]()

func fail(n *Node, path, format string, args ...any) error {
	msg := fmt.Sprintf(format, args...)
	if path != "" {
		msg = path + ": " + msg
	}
	return &Error{Line: n.Line, Column: n.Column, Msg: msg}
}

func join(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}

func decode(n *Node, v reflect.Value, path string) error {
	if v.CanAddr() && v.Addr().Type().Implements(unmarshalerType) {
		if n.IsNull() {
			return nil
		}
		if err := v.Addr().Interface().(Unmarshaler).UnmarshalYAML(n); err != nil {
			var pos *Error
			if errors.As(err, &pos) {
				return err
			}
			return fail(n, path, "%v", err)
		}
		return nil
	}
	if n.IsNull() && v.Kind() != reflect.String {
		v.SetZero()
		return nil
	}
	switch v.Kind() {
	case reflect.Pointer:
		elem := reflect.New(v.Type().Elem())
		if err := decode(n, elem.Elem(), path); err != nil {
			return err
		}
		v.Set(elem)
		return nil
	case reflect.Struct:
		return decodeStruct(n, v, path)
	case reflect.Map:
		if n.Kind != MapNode {
			return fail(n, path, "want a mapping")
		}
		if v.Type().Key().Kind() != reflect.String {
			return fail(n, path, "yaml: map keys must be strings")
		}
		m := reflect.MakeMapWithSize(v.Type(), len(n.Pairs))
		for _, p := range n.Pairs {
			elem := reflect.New(v.Type().Elem()).Elem()
			if err := decode(p.Value, elem, join(path, p.Key.Value)); err != nil {
				return err
			}
			m.SetMapIndex(reflect.ValueOf(p.Key.Value).Convert(v.Type().Key()), elem)
		}
		v.Set(m)
		return nil
	case reflect.Slice:
		if n.Kind != SeqNode {
			return fail(n, path, "want a list")
		}
		s := reflect.MakeSlice(v.Type(), len(n.Items), len(n.Items))
		for i, item := range n.Items {
			if err := decode(item, s.Index(i), fmt.Sprintf("%s[%d]", path, i)); err != nil {
				return err
			}
		}
		v.Set(s)
		return nil
	}

	if n.Kind != ScalarNode {
		return fail(n, path, "want a single value, not a %s", kindName(n.Kind))
	}
	switch v.Kind() {
	case reflect.String:
		if n.IsNull() {
			v.SetString("")
		} else {
			v.SetString(n.Value)
		}
	case reflect.Bool:
		switch {
		case n.Style == Plain && n.Value == "true":
			v.SetBool(true)
		case n.Style == Plain && n.Value == "false":
			v.SetBool(false)
		default:
			return fail(n, path, "want true or false, got %q", n.Value)
		}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		i, err := strconv.ParseInt(n.Value, 10, 64)
		if err != nil || n.Style != Plain || v.OverflowInt(i) {
			return fail(n, path, "want a whole number, got %q", n.Value)
		}
		v.SetInt(i)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		u, err := strconv.ParseUint(n.Value, 10, 64)
		if err != nil || n.Style != Plain || v.OverflowUint(u) {
			return fail(n, path, "want a whole number of at least 0, got %q", n.Value)
		}
		v.SetUint(u)
	case reflect.Float32, reflect.Float64:
		f, err := strconv.ParseFloat(n.Value, 64)
		if err != nil || n.Style != Plain || math.IsNaN(f) || math.IsInf(f, 0) {
			return fail(n, path, "want a number, got %q", n.Value)
		}
		v.SetFloat(f)
	default:
		return fail(n, path, "yaml: can't decode into %s", v.Type())
	}
	return nil
}

func decodeStruct(n *Node, v reflect.Value, path string) error {
	if n.Kind != MapNode {
		return fail(n, path, "want a mapping, not a %s", kindName(n.Kind))
	}
	fields := map[string]int{}
	t := v.Type()
	for i := range t.NumField() {
		tag, _, _ := strings.Cut(t.Field(i).Tag.Get("yaml"), ",")
		if tag != "" && tag != "-" && t.Field(i).IsExported() {
			fields[tag] = i
		}
	}
	for _, p := range n.Pairs {
		i, ok := fields[p.Key.Value]
		if !ok {
			return fail(p.Key, join(path, p.Key.Value), "unknown field")
		}
		if err := decode(p.Value, v.Field(i), join(path, p.Key.Value)); err != nil {
			return err
		}
	}
	return nil
}

func kindName(k Kind) string {
	switch k {
	case MapNode:
		return "mapping"
	case SeqNode:
		return "list"
	}
	return "scalar"
}
