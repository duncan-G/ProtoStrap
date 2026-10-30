package configfiles

import (
	"strings"
	"testing"
)

func TestDigest(t *testing.T) {
	a := Files{"lds.yaml": []byte("listeners"), "cds.yaml": []byte("clusters")}
	d := Digest(a)
	if !ValidDigest(d) {
		t.Fatalf("digest %q isn't valid", d)
	}
	if Digest(Files{"cds.yaml": []byte("clusters"), "lds.yaml": []byte("listeners")}) != d {
		t.Errorf("the same files gave another digest")
	}
	for name, other := range map[string]Files{
		"content":         {"lds.yaml": []byte("listeners!"), "cds.yaml": []byte("clusters")},
		"name":            {"lds.yml": []byte("listeners"), "cds.yaml": []byte("clusters")},
		"a file moved":    {"x/lds.yaml": []byte("listeners"), "cds.yaml": []byte("clusters")},
		"bytes regrouped": {"lds.yaml": []byte("listenersc"), "ds.yaml": []byte("clusters")},
		"one file fewer":  {"lds.yaml": []byte("listeners")},
	} {
		if Digest(other) == d {
			t.Errorf("%s: same digest", name)
		}
	}
	for _, bad := range []string{"", "abc", strings.Repeat("g", 32), strings.Repeat("A", 32)} {
		if ValidDigest(bad) {
			t.Errorf("ValidDigest(%q)", bad)
		}
	}
}

func TestValidate(t *testing.T) {
	if err := (Files{"pg_hba.conf": []byte("x"), "conf.d/a.conf": []byte(""), ".hidden": []byte("y")}).Validate(); err != nil {
		t.Errorf("valid files: %v", err)
	}
	for name, f := range map[string]Files{
		"none":              {},
		"absolute":          {"/etc/passwd": []byte("x")},
		"escapes":           {"../x": []byte("x")},
		"escapes midway":    {"a/../../x": []byte("x")},
		"untidy":            {"a//b": []byte("x")},
		"trailing slash":    {"a/": []byte("x")},
		"dot":               {".": []byte("x")},
		"backslash":         {`a\b`: []byte("x")},
		"nul":               {"a\x00b": []byte("x")},
		"agent's own name":  {"..data": []byte("x")},
		"agent's own, deep": {"a/..v-1/b": []byte("x")},
		"file and dir":      {"a": []byte("x"), "a/b": []byte("y")},
		"too large":         {"a": make([]byte, MaxBytes+1)},
		"too many": func() Files {
			f := Files{}
			for i := range MaxFiles + 1 {
				f[strings.Repeat("a", 1+i%7)+"/"+string(rune('a'+i%26))+strings.Repeat("b", i/26)] = nil
			}
			return f
		}(),
	} {
		if err := f.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
