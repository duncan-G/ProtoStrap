package reporter

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Insights is CloudWatch Logs: its log groups, and Logs Insights queries over them.
type Insights interface {
	Groups(ctx context.Context, prefix string) ([]string, error)
	Query(ctx context.Context, groups []string, query string, from, to time.Time, poll time.Duration) ([]map[string]string, error)
}

// CloudWatch reads the components' errors from their log groups in CloudWatch Logs, where
// Docker sends their output when the application runs no telemetry.
type CloudWatch struct {
	Logs Insights
	App  string
	// Poll is how often a query's results are asked for. It defaults to 2 seconds.
	Poll time.Duration
}

// insightsQuery finds lines that look like errors, as errorQuery does in Loki.
const insightsQuery = `fields @timestamp, @message, @log | filter @message like /(?i)(error|exception|panic|fatal|traceback|unhandled|critical)/ | sort @timestamp desc | limit 5000`

func (c *CloudWatch) Errors(ctx context.Context, from, to time.Time) ([]LogLine, error) {
	prefix := "/protostrap/" + c.App + "/"
	groups, err := c.Logs.Groups(ctx, prefix)
	if err != nil {
		return nil, fmt.Errorf("listing the log groups: %w", err)
	}
	if len(groups) == 0 {
		return nil, nil
	}
	poll := c.Poll
	if poll == 0 {
		poll = 2 * time.Second
	}
	rows, err := c.Logs.Query(ctx, groups, insightsQuery, from, to, poll)
	if err != nil {
		return nil, fmt.Errorf("querying CloudWatch Logs: %w", err)
	}
	var lines []LogLine
	for _, row := range rows {
		at, err := time.Parse("2006-01-02 15:04:05.000", row["@timestamp"])
		if err != nil {
			continue
		}
		// @log is the account and the group, such as 123456789012:/protostrap/shop/api.
		group := row["@log"][strings.Index(row["@log"], ":")+1:]
		lines = append(lines, LogLine{Time: at.UTC(), Component: strings.TrimPrefix(group, prefix), Message: row["@message"]})
	}
	return lines, nil
}
