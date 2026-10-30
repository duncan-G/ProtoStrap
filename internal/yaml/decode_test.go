package yaml

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

type size int

// UnmarshalYAML shows custom decoding: a size is the number of characters before its final
// x, so "xxxx" is 3.
func (s *size) UnmarshalYAML(n *Node) error {
	v, ok := strings.CutSuffix(n.Value, "x")
	if !ok || n.Kind != ScalarNode {
		return errors.New(`want a size like "3x"`)
	}
	*s = size(len(v))
	return nil
}

type inner struct {
	Name string `yaml:"name"`
	Size size   `yaml:"size"`
}

type outer struct {
	App     string            `yaml:"app"`
	Count   int               `yaml:"count"`
	Big     int64             `yaml:"big"`
	Ratio   float64           `yaml:"ratio"`
	On      bool              `yaml:"on"`
	Tags    []string          `yaml:"tags"`
	Env     map[string]string `yaml:"env"`
	Inner   inner             `yaml:"inner"`
	Ptr     *inner            `yaml:"ptr"`
	Items   []inner           `yaml:"items"`
	ByName  map[string]*inner `yaml:"by_name"`
	Skipped string            `yaml:"-"`
	NoTag   string
}

func TestDecode(t *testing.T) {
	src := `app: shop
count: 3
big: 9000000000
ratio: 0.75
on: true
tags: [a, "b"]
env: { A: "1", B: yes }
inner: { name: x, size: xxxx }
ptr:
  name: p
items:
  - name: i1
  - { name: i2, size: xx }
by_name:
  one: { name: o }
  two:
`
	n, err := Parse([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	var got outer
	if err := Decode(n, &got); err != nil {
		t.Fatal(err)
	}
	want := outer{
		App: "shop", Count: 3, Big: 9000000000, Ratio: 0.75, On: true,
		Tags: []string{"a", "b"}, Env: map[string]string{"A": "1", "B": "yes"},
		Inner: inner{Name: "x", Size: 3}, Ptr: &inner{Name: "p"},
		Items:  []inner{{Name: "i1"}, {Name: "i2", Size: 1}},
		ByName: map[string]*inner{"one": {Name: "o"}, "two": nil},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got  %+v\nwant %+v", got, want)
	}
}

func TestDecodeErrors(t *testing.T) {
	tests := []struct {
		name, src, want string
	}{
		{"unknown field", "app: x\nnope: 1\n", `line 2, column 1: nope: unknown field`},
		{"unknown nested field", "inner:\n  name: x\n  colour: red\n", `line 3, column 3: inner.colour: unknown field`},
		{"not a number", "count: lots\n", `line 1, column 8: count: want a whole number, got "lots"`},
		{"quoted number", `count: "3"` + "\n", `count: want a whole number`},
		{"fraction for int", "count: 1.5\n", `count: want a whole number`},
		{"not a bool", "on: yes\n", `on: want true or false`},
		{"map for string", "app: { a: 1 }\n", `app: want a single value`},
		{"seq for map", "env: [a]\n", `env: want a mapping`},
		{"scalar for seq", "tags: a\n", `tags: want a list`},
		{"custom error", "inner: { size: 3 }\n", `inner.size: want a size like "3x"`},
		{"error in list item", "items:\n  - name: a\n  - name: [x]\n", `line 3, column 11: items[1].name: want a single value`},
		{"error in map value", "by_name:\n  one: { name: [x] }\n", `by_name.one.name: want a single value`},
		{"top level not a map", "- a\n", `want a mapping`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			n, err := Parse([]byte(tt.src))
			if err != nil {
				t.Fatal(err)
			}
			var v outer
			err = Decode(n, &v)
			if err == nil {
				t.Fatalf("no error")
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error %q doesn't contain %q", err, tt.want)
			}
		})
	}
}

func TestDecodeNullDocument(t *testing.T) {
	n, err := Parse(nil)
	if err != nil {
		t.Fatal(err)
	}
	var v outer
	if err := Decode(n, &v); err != nil {
		t.Errorf("an empty document decodes to the zero value, got %v", err)
	}
}
