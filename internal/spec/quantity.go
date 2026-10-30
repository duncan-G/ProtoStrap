package spec

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/duncan-G/protostrap/internal/yaml"
)

// CPU is millicores: "500m" or "2" (cores).
type CPU int64

// Memory is MiB: "512Mi", "1Gi", "1.5Gi".
type Memory int64

// Size is a volume's size in GiB: "100Gi", "1Ti".
type Size int64

// Percent is a fraction: "70%" is 0.70.
type Percent float64

// Rate is per second: "2/s", "30/m", "3600/h".
type Rate float64

// Duration is "30s", "2m", "1h30m", or whole days, "4d".
type Duration struct{ time.Duration }

func scalar(n *yaml.Node) (string, error) {
	if n.Kind != yaml.ScalarNode {
		return "", errors.New("want a single value")
	}
	return n.Value, nil
}

func (c *CPU) UnmarshalYAML(n *yaml.Node) error {
	s, err := scalar(n)
	if err == nil {
		var v int64
		v, err = parseCPU(s)
		*c = CPU(v)
	}
	return err
}

func (m *Memory) UnmarshalYAML(n *yaml.Node) error {
	s, err := scalar(n)
	if err == nil {
		var v int64
		v, err = parseMemory(s)
		*m = Memory(v)
	}
	return err
}

func (z *Size) UnmarshalYAML(n *yaml.Node) error {
	s, err := scalar(n)
	if err == nil {
		var v int64
		v, err = parseSize(s)
		*z = Size(v)
	}
	return err
}

func (p *Percent) UnmarshalYAML(n *yaml.Node) error {
	s, err := scalar(n)
	if err == nil {
		var v float64
		v, err = parsePercent(s)
		*p = Percent(v)
	}
	return err
}

func (r *Rate) UnmarshalYAML(n *yaml.Node) error {
	s, err := scalar(n)
	if err == nil {
		var v float64
		v, err = parseRate(s)
		*r = Rate(v)
	}
	return err
}

func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	s, err := scalar(n)
	if err == nil {
		d.Duration, err = parseDuration(s)
	}
	return err
}

// String writes a duration the way the spec does.
func (d Duration) String() string { return d.Duration.String() }

// number parses a non-negative decimal, refusing exponents, signs and the like.
func number(s string) (float64, bool) {
	if s == "" || strings.ContainsAny(s, "eE+-_ ") {
		return 0, false
	}
	f, err := strconv.ParseFloat(s, 64)
	return f, err == nil && f >= 0 && !math.IsInf(f, 0)
}

// whole converts f to an integer if it is one, allowing for float rounding.
func whole(f float64) (int64, bool) {
	r := math.Round(f)
	return int64(r), math.Abs(f-r) < 1e-9
}

func parseCPU(s string) (int64, error) {
	fail := fmt.Errorf(`want CPU as millicores like "500m" or cores like "2", got %q`, s)
	if m, ok := strings.CutSuffix(s, "m"); ok {
		v, ok := number(m)
		n, isWhole := whole(v)
		if !ok || !isWhole {
			return 0, fail
		}
		return n, nil
	}
	v, ok := number(s)
	n, isWhole := whole(v * 1000)
	if !ok || !isWhole {
		return 0, fail
	}
	return n, nil
}

func parseMemory(s string) (int64, error) {
	units := map[string]float64{"Ki": 1.0 / 1024, "Mi": 1, "Gi": 1024, "Ti": 1024 * 1024}
	for suffix, mib := range units {
		if v, ok := strings.CutSuffix(s, suffix); ok {
			f, ok := number(v)
			n, isWhole := whole(f * mib)
			if ok && isWhole {
				return n, nil
			}
		}
	}
	return 0, fmt.Errorf(`want memory with a binary unit, like "512Mi" or "1Gi", got %q`, s)
}

func parseSize(s string) (int64, error) {
	units := map[string]float64{"Gi": 1, "Ti": 1024}
	for suffix, gib := range units {
		if v, ok := strings.CutSuffix(s, suffix); ok {
			f, ok := number(v)
			n, isWhole := whole(f * gib)
			if ok && isWhole && n > 0 {
				return n, nil
			}
		}
	}
	return 0, fmt.Errorf(`want a whole number of GiB, like "100Gi", got %q`, s)
}

func parsePercent(s string) (float64, error) {
	v, ok := strings.CutSuffix(s, "%")
	f, valid := number(v)
	if !ok || !valid {
		return 0, fmt.Errorf(`want a percentage like "70%%", got %q`, s)
	}
	return f / 100, nil
}

func parseRate(s string) (float64, error) {
	count, unit, ok := strings.Cut(s, "/")
	per := map[string]float64{"s": 1, "m": 60, "min": 60, "h": 3600}[unit]
	f, valid := number(count)
	if !ok || per == 0 || !valid {
		return 0, fmt.Errorf(`want a rate like "2/s", "30/m" or "100/h", got %q`, s)
	}
	return f / per, nil
}

func parseDuration(s string) (time.Duration, error) {
	fail := fmt.Errorf(`want a duration like "30s", "2m" or "4d", got %q`, s)
	if days, ok := strings.CutSuffix(s, "d"); ok {
		n, err := strconv.Atoi(days)
		if err != nil || n < 0 {
			return 0, fail
		}
		return time.Duration(n) * 24 * time.Hour, nil
	}
	d, err := time.ParseDuration(s)
	if err != nil || d < 0 {
		return 0, fail
	}
	return d, nil
}

// FormatMemory writes MiB the way the spec does.
func FormatMemory(mib int64) string {
	if mib%1024 == 0 && mib > 0 {
		return strconv.FormatInt(mib/1024, 10) + "Gi"
	}
	return strconv.FormatInt(mib, 10) + "Mi"
}

// FormatCPU writes millicores the way the spec does.
func FormatCPU(m int64) string { return strconv.FormatInt(m, 10) + "m" }
