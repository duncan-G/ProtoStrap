// Package metrics scrapes the Prometheus metrics the scheduler asks agents for: the
// gauges behind `metric` signals and limits, and Envoy's request counts.
package metrics

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Sample is one series' value.
type Sample struct {
	Name   string
	Labels map[string]string
	Value  float64
}

var metricName = regexp.MustCompile(`^[a-zA-Z_:][a-zA-Z0-9_:]*$`)

// Parse reads the Prometheus text exposition format.
func Parse(r io.Reader) ([]Sample, error) {
	var out []Sample
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		s, err := parseLine(line)
		if err != nil {
			return nil, fmt.Errorf("metrics line %d: %w", n, err)
		}
		out = append(out, s)
	}
	return out, sc.Err()
}

func parseLine(line string) (Sample, error) {
	end := strings.IndexAny(line, "{ \t")
	if end < 0 {
		return Sample{}, errors.New("no value")
	}
	s := Sample{Name: line[:end]}
	if !metricName.MatchString(s.Name) {
		return Sample{}, fmt.Errorf("bad name %q", s.Name)
	}
	rest := strings.TrimLeft(line[end:], " \t")
	if strings.HasPrefix(rest, "{") {
		labels, after, err := parseLabels(rest[1:])
		if err != nil {
			return Sample{}, err
		}
		s.Labels, rest = labels, after
	}
	fields := strings.Fields(rest)
	if len(fields) == 0 || len(fields) > 2 {
		return Sample{}, errors.New("no value")
	}
	v, err := parseValue(fields[0])
	if err != nil {
		return Sample{}, err
	}
	s.Value = v
	return s, nil
}

func parseValue(s string) (float64, error) {
	switch s {
	case "+Inf":
		return math.Inf(1), nil
	case "-Inf":
		return math.Inf(-1), nil
	case "NaN":
		return math.NaN(), nil
	}
	return strconv.ParseFloat(s, 64)
}

// parseLabels reads `name="value", ...}` and returns what follows the brace.
func parseLabels(s string) (map[string]string, string, error) {
	labels := map[string]string{}
	for {
		s = strings.TrimLeft(s, " \t")
		if strings.HasPrefix(s, "}") {
			return labels, s[1:], nil
		}
		eq := strings.IndexByte(s, '=')
		if eq < 0 {
			return nil, "", errors.New("unterminated labels")
		}
		name := strings.TrimSpace(s[:eq])
		if !metricName.MatchString(name) {
			return nil, "", fmt.Errorf("bad label name %q", name)
		}
		s = strings.TrimLeft(s[eq+1:], " \t")
		if !strings.HasPrefix(s, `"`) {
			return nil, "", fmt.Errorf("label %s: value not quoted", name)
		}
		var value strings.Builder
		i := 1
		for ; i < len(s) && s[i] != '"'; i++ {
			if s[i] == '\\' && i+1 < len(s) {
				i++
				switch s[i] {
				case 'n':
					value.WriteByte('\n')
				default:
					value.WriteByte(s[i])
				}
				continue
			}
			value.WriteByte(s[i])
		}
		if i >= len(s) {
			return nil, "", errors.New("unterminated label value")
		}
		labels[name] = value.String()
		s = strings.TrimLeft(s[i+1:], " \t")
		s = strings.TrimPrefix(s, ",")
	}
}

// Selector picks series: a name, and labels they must have.
type Selector struct {
	Name   string
	Labels map[string]string
}

// ParseSelector reads `name` or `name{label="value",...}`.
func ParseSelector(s string) (Selector, error) {
	name, rest, hasLabels := strings.Cut(s, "{")
	if !metricName.MatchString(name) {
		return Selector{}, fmt.Errorf("bad metric selector %q", s)
	}
	sel := Selector{Name: name}
	if hasLabels {
		labels, after, err := parseLabels(rest)
		if err != nil || strings.TrimSpace(after) != "" {
			return Selector{}, fmt.Errorf("bad metric selector %q", s)
		}
		sel.Labels = labels
	}
	return sel, nil
}

func (sel Selector) matches(s Sample) bool {
	if s.Name != sel.Name {
		return false
	}
	for k, v := range sel.Labels {
		if s.Labels[k] != v {
			return false
		}
	}
	return true
}

// Select returns a value for each selector that matches: the series without labels if
// there is one, or else the sum of the matching series. Values that aren't finite are left
// out, as JSON can't carry them.
func Select(samples []Sample, selectors []string) map[string]float64 {
	out := map[string]float64{}
	for _, raw := range selectors {
		sel, err := ParseSelector(raw)
		if err != nil {
			continue
		}
		var sum float64
		var found, bare bool
		for _, s := range samples {
			if !sel.matches(s) {
				continue
			}
			if len(sel.Labels) == 0 && len(s.Labels) == 0 {
				sum, found, bare = s.Value, true, true
				break
			}
			if !bare {
				sum += s.Value
				found = true
			}
		}
		if found && !math.IsNaN(sum) && !math.IsInf(sum, 0) {
			out[raw] = sum
		}
	}
	return out
}

// ParseEndpoint reads a spec's `metrics` endpoint: "port/path", or just "port" for
// /metrics.
func ParseEndpoint(endpoint string) (int, string, error) {
	portStr, path, hasPath := strings.Cut(endpoint, "/")
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 1 || port > 65535 {
		return 0, "", fmt.Errorf("bad metrics endpoint %q", endpoint)
	}
	if !hasPath || path == "" {
		return port, "/metrics", nil
	}
	return port, "/" + path, nil
}

// maxBody bounds what is read from an endpoint.
const maxBody = 4 << 20

// Scraper reads replicas' endpoints.
type Scraper struct{ client *http.Client }

func NewScraper(timeout time.Duration) *Scraper {
	return &Scraper{client: &http.Client{Timeout: timeout, Transport: &http.Transport{Proxy: nil}}}
}

// Scrape reads the named metrics from a replica's endpoint.
func (s *Scraper) Scrape(ctx context.Context, addr, endpoint string, names []string) (map[string]float64, error) {
	port, path, err := ParseEndpoint(endpoint)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+net.JoinHostPort(addr, strconv.Itoa(port))+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "text/plain;version=0.0.4")
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("metrics %s: HTTP %d", path, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return nil, err
	}
	if len(body) > maxBody {
		return nil, fmt.Errorf("metrics %s: larger than %d bytes", path, maxBody)
	}
	samples, err := Parse(strings.NewReader(string(body)))
	if err != nil {
		return nil, err
	}
	return Select(samples, names), nil
}
