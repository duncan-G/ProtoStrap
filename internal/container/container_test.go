package container

import (
	"reflect"
	"testing"
)

func TestParseCPUSet(t *testing.T) {
	cases := map[string][]int{
		"":          nil,
		"0":         {0},
		"0-2":       {0, 1, 2},
		"0,2-3":     {0, 2, 3},
		"3,1":       {1, 3},
		" 1 , 4-5 ": {1, 4, 5},
		"2-2":       {2},
	}
	for in, want := range cases {
		got, err := ParseCPUSet(in)
		if err != nil {
			t.Errorf("ParseCPUSet(%q): %v", in, err)
			continue
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("ParseCPUSet(%q) = %v, want %v", in, got, want)
		}
	}
	for _, bad := range []string{"a", "3-1", "1-", "-1", "1,,2"} {
		if _, err := ParseCPUSet(bad); err == nil {
			t.Errorf("ParseCPUSet(%q) should fail", bad)
		}
	}
}

func TestFormatCPUSet(t *testing.T) {
	cases := map[string][]int{
		"":        nil,
		"0":       {0},
		"0-2":     {2, 0, 1},
		"0,2-3":   {0, 2, 3},
		"1,3,5-7": {1, 3, 5, 6, 7},
	}
	for want, in := range cases {
		if got := FormatCPUSet(in); got != want {
			t.Errorf("FormatCPUSet(%v) = %q, want %q", in, got, want)
		}
	}
}

func TestContainerLabels(t *testing.T) {
	c := Container{Labels: map[string]string{LabelReplica: "api-1", LabelComponent: "api"}}
	if c.Replica() != "api-1" || c.Label(LabelComponent) != "api" || c.Label("missing") != "" {
		t.Fatalf("labels: %+v", c)
	}
	if (Container{}).Replica() != "" {
		t.Fatal("a container without labels has no replica")
	}
}

func TestStatusRunning(t *testing.T) {
	for s, want := range map[Status]bool{Running: true, Paused: true, Restarting: false, Exited: false, Created: false, Dead: false} {
		if s.Up() != want {
			t.Errorf("%s.Up() = %v", s, !want)
		}
	}
}
