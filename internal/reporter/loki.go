package reporter

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Loki reads the components' errors from telemetry's Loki, through Grafana's data source
// proxy on its socket on data-1.
type Loki struct {
	Client *http.Client
	// Base is Grafana's address, such as http://grafana when Client dials its socket.
	Base string
}

// errorQuery finds lines that look like errors in every component's logs.
const errorQuery = `{service_name=~".+"} |~ "(?i)(error|exception|panic|fatal|traceback|unhandled|critical)"`

// errorLimit is the most lines one report reads, the newest first.
const errorLimit = 5000

func (l *Loki) Errors(ctx context.Context, from, to time.Time) ([]LogLine, error) {
	q := url.Values{
		"query":     {errorQuery},
		"start":     {strconv.FormatInt(from.UnixNano(), 10)},
		"end":       {strconv.FormatInt(to.UnixNano(), 10)},
		"limit":     {strconv.Itoa(errorLimit)},
		"direction": {"backward"},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, l.Base+"/api/datasources/proxy/uid/loki/loki/api/v1/query_range?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := l.Client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("querying Loki: %w", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, fmt.Errorf("reading Loki's answer: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("Loki answered %d: %s", resp.StatusCode, strings.TrimSpace(string(body[:min(len(body), 500)])))
	}
	var out struct {
		Data struct {
			Result []struct {
				Stream map[string]string `json:"stream"`
				Values [][2]string       `json:"values"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("Loki's answer: %w", err)
	}
	var lines []LogLine
	for _, stream := range out.Data.Result {
		for _, v := range stream.Values {
			ns, err := strconv.ParseInt(v[0], 10, 64)
			if err != nil {
				continue
			}
			lines = append(lines, LogLine{Time: time.Unix(0, ns).UTC(), Component: stream.Stream["service_name"], Message: v[1]})
		}
	}
	return lines, nil
}
