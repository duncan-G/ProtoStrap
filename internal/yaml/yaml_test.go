package yaml

import (
	"strings"
	"testing"
)

// render prints a tree compactly, so a test can compare the whole parse in one string:
// maps as {k: v, ...}, sequences as [a, b], plain scalars bare, quoted ones in "", and
// null as ~.
func render(n *Node) string {
	switch n.Kind {
	case MapNode:
		var parts []string
		for _, p := range n.Pairs {
			parts = append(parts, p.Key.Value+": "+render(p.Value))
		}
		return "{" + strings.Join(parts, ", ") + "}"
	case SeqNode:
		var parts []string
		for _, item := range n.Items {
			parts = append(parts, render(item))
		}
		return "[" + strings.Join(parts, ", ") + "]"
	default:
		if n.IsNull() {
			return "~"
		}
		if n.Style != Plain {
			return `"` + n.Value + `"`
		}
		return n.Value
	}
}

func parse(t *testing.T, src string) *Node {
	t.Helper()
	n, err := Parse([]byte(src))
	if err != nil {
		t.Fatalf("Parse: %v\n%s", err, src)
	}
	return n
}

func TestParse(t *testing.T) {
	tests := []struct {
		name string
		src  string
		want string
	}{
		{"empty document", "", "~"},
		{"comment only", "# nothing\n\n", "~"},
		{"scalar document", "hello", "hello"},
		{"flat map", "app: shop\nprotostrap: 1.4.0\n", "{app: shop, protostrap: 1.4.0}"},
		{"nested map", "pools:\n  data:\n    vm: m7g.xlarge\n    count: 1\n", "{pools: {data: {vm: m7g.xlarge, count: 1}}}"},
		{"null value", "a:\nb: 1\n", "{a: ~, b: 1}"},
		{"tilde and null", "a: ~\nb: null\nc: Null\n", "{a: ~, b: ~, c: ~}"},
		{"sequence under key, indented", "hosts:\n  - a.com\n  - b.com\n", "{hosts: [a.com, b.com]}"},
		{"sequence under key, same indent", "hosts:\n- a.com\n- b.com\nnext: 1\n", "{hosts: [a.com, b.com], next: 1}"},
		{"flow sequence", "vm: [m7g.xlarge, m6g.xlarge]\n", "{vm: [m7g.xlarge, m6g.xlarge]}"},
		{"flow map", "request: { cpu: 500m, memory: 512Mi }\n", "{request: {cpu: 500m, memory: 512Mi}}"},
		{"nested flow", "demand: [{ queue: orders, rate: 2/s, drain: 60s }]\n", "{demand: [{queue: orders, rate: 2/s, drain: 60s}]}"},
		{"empty flow collections", "a: []\nb: {}\n", "{a: [], b: {}}"},
		{"flow across lines", "a: [x,\n  y,\n  z]\nb: 1\n", "{a: [x, y, z], b: 1}"},
		{"trailing comma in flow", "a: [x, y,]\n", "{a: [x, y]}"},
		{"sequence of maps", "items:\n  - name: a\n    size: 1\n  - name: b\n", "{items: [{name: a, size: 1}, {name: b}]}"},
		{"sequence of maps, same indent", "items:\n- name: a\n  size: 1\n- name: b\n", "{items: [{name: a, size: 1}, {name: b}]}"},
		{"sequence of flow maps", "d:\n  - { cpu: 70% }\n  - { metric: lag, target: 0.05 }\n", "{d: [{cpu: 70%}, {metric: lag, target: 0.05}]}"},
		{"nested sequences", "a:\n  - - x\n    - y\n  - - z\n", "{a: [[x, y], [z]]}"},
		{"map in map in seq", "a:\n  - b:\n      c: 1\n", "{a: [{b: {c: 1}}]}"},
		{"comments everywhere", "# head\na: 1 # one\n# mid\nb: [x, y] # two\n", "{a: 1, b: [x, y]}"},
		{"hash inside a plain scalar", "a: x#y\n", "{a: x#y}"},
		{"double quoted", `a: "x: y # z"` + "\n", `{a: "x: y # z"}`},
		{"double quoted escapes", `a: "tab\there \"q\" \\ \u00e9"` + "\n", "{a: \"tab\there \"q\" \\ é\"}"},
		{"single quoted", "a: 'it''s'\n", `{a: "it's"}`},
		{"quoted dash", `b: [redis-cli, --rdb, "-"]` + "\n", `{b: [redis-cli, --rdb, "-"]}`},
		{"quoted key", `"a b": 1` + "\n", "{a b: 1}"},
		{"colon without space stays in scalar", "a: http://api:8080\n", "{a: http://api:8080}"},
		{"url in flow", "a: [http://x:1/y]\n", "{a: [http://x:1/y]}"},
		{"document start marker", "---\na: 1\n", "{a: 1}"},
		{"windows line endings", "a: 1\r\nb: 2\r\n", "{a: 1, b: 2}"},
		{"literal block scalar", "a: |\n  line one\n  line two\nb: 1\n", "{a: \"line one\nline two\n\", b: 1}"},
		{"literal block scalar strip", "a: |-\n  x\n  y\n", "{a: \"x\ny\"}"},
		{"folded block scalar", "a: >\n  one\n  two\n\n  three\n", "{a: \"one two\nthree\n\"}"},
		{"block scalar keeps inner indent", "a: |\n  x\n    y\n", "{a: \"x\n  y\n\"}"},
		{"value with spaces", "a: hello world\n", "{a: hello world}"},
		{"dash in plain scalar", "a: -1\nb: --flag\n", "{a: -1, b: --flag}"},
		{"blank lines between entries", "a: 1\n\n\nb: 2\n", "{a: 1, b: 2}"},
		{"deep indentation change", "a:\n    b:\n        c: 1\n    d: 2\ne: 3\n", "{a: {b: {c: 1}, d: 2}, e: 3}"},
		{"top-level sequence", "- a\n- b\n", "[a, b]"},
		{"empty sequence item", "a:\n  -\n  - b\n", "{a: [~, b]}"},
		{"percent and slash", "target: 70%\nrate: 2/s\n", "{target: 70%, rate: 2/s}"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := render(parse(t, tt.src)); got != tt.want {
				t.Errorf("got  %s\nwant %s", got, tt.want)
			}
		})
	}
}

func TestParseErrors(t *testing.T) {
	tests := []struct {
		name string
		src  string
		// want is a substring of the error. Most cases include the line it names, as "line 2".
		want string
	}{
		{"tab indentation", "a:\n\tb: 1\n", "line 2: tabs"},
		{"duplicate key", "a: 1\na: 2\n", "line 2: duplicate key \"a\""},
		{"duplicate key in flow", "a: {x: 1, x: 2}\n", "duplicate key \"x\""},
		{"bad indentation", "a:\n  b: 1\n c: 2\n", "line 3"},
		{"unclosed flow", "a: [x, y\nb: 1\n", "unclosed"},
		{"unclosed quote", "a: \"x\n", "line 1: unclosed"},
		{"anchor", "a: &x 1\n", "anchors"},
		{"alias", "a: *x\n", "aliases"},
		{"tag", "a: !!str 1\n", "tags"},
		{"text after flow", "a: [x] y\n", "line 1"},
		{"text after quoted", `a: "x" y` + "\n", "line 1"},
		{"mapping value in plain scalar", "a: b: c\n", "line 1"},
		{"sequence where map continues", "a: 1\n- b\n", "line 2"},
		{"map key without value separator", "a: 1\nb\n", "line 2"},
		{"second document", "a: 1\n---\nb: 2\n", "one document"},
		{"merge key", "<<: {a: 1}\n", "merge"},
		{"complex key", "? a\n: b\n", "line 1"},
		{"unclosed flow map", "a: {x: 1\n", "unclosed"},
		{"flow map entry without colon", "a: {x}\n", "line 1"},
		{"bad escape", `a: "\q"` + "\n", "escape"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse([]byte(tt.src))
			if err == nil {
				t.Fatalf("no error for\n%s", tt.src)
			}
			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("error %q doesn't contain %q", err, tt.want)
			}
		})
	}
}

func TestPositions(t *testing.T) {
	n := parse(t, "app: shop\ncomponents:\n  api:\n    port: 8080\n    vm: [a,  b]\n")
	api := n.Get("components").Get("api")
	if api == nil {
		t.Fatal("no components.api")
	}
	if api.Line != 4 {
		t.Errorf("api map at line %d, want 4", api.Line)
	}
	port := api.Get("port")
	if port.Line != 4 || port.Column != 11 {
		t.Errorf("port at %d:%d, want 4:11", port.Line, port.Column)
	}
	vm := api.Get("vm")
	if vm.Items[1].Line != 5 || vm.Items[1].Column != 14 {
		t.Errorf("vm[1] at %d:%d, want 5:14", vm.Items[1].Line, vm.Items[1].Column)
	}
	if k := api.KeyNode("port"); k == nil || k.Line != 4 || k.Column != 5 {
		t.Errorf("key port at %+v, want 4:5", k)
	}
}

func TestKeyOrderKept(t *testing.T) {
	n := parse(t, "z: 1\na: 2\nm: 3\n")
	var keys []string
	for _, p := range n.Pairs {
		keys = append(keys, p.Key.Value)
	}
	if strings.Join(keys, ",") != "z,a,m" {
		t.Errorf("keys %v, want z,a,m", keys)
	}
}

func TestParseTheSpecExample(t *testing.T) {
	// The source is a cut-down copy of the example spec in docs/protostrap.md, which every
	// part of ProtoStrap reads.
	src := `app: shop
protostrap: 1.4.0
hosts: [shop.example.com]

pools:
  ingress: { vm: c8g.medium }
  general:
    vm: [m7g.xlarge, m6g.xlarge, m8g.xlarge, m7gd.xlarge]
    min: 1
    max: 10
    spare: 1
    target: 70%
    reserved: { cpu: 400m, memory: 1Gi }
  data: { vm: m7g.xlarge, count: 1 }

components:
  api:
    kind: service
    routes: [shop.example.com/api/]
    migrate: [dotnet, Api.dll, migrate]
    scale:
      min: 2
      demand: [{ cpu: 70% }]
  redis:
    backup: [redis-cli, --rdb, "-"]
`
	n := parse(t, src)
	want := `{app: shop, protostrap: 1.4.0, hosts: [shop.example.com], pools: {ingress: {vm: c8g.medium}, general: {vm: [m7g.xlarge, m6g.xlarge, m8g.xlarge, m7gd.xlarge], min: 1, max: 10, spare: 1, target: 70%, reserved: {cpu: 400m, memory: 1Gi}}, data: {vm: m7g.xlarge, count: 1}}, components: {api: {kind: service, routes: [shop.example.com/api/], migrate: [dotnet, Api.dll, migrate], scale: {min: 2, demand: [{cpu: 70%}]}}, redis: {backup: [redis-cli, --rdb, "-"]}}}`
	if got := render(n); got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}
