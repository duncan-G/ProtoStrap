// Package reporter writes the daily report (docs/protostrap.md#reporter): what happened in
// the last day, and what to change. It reads the infra log from the deploy bucket, the
// scheduler's decisions and signal snapshots and the agents' warnings, and every
// component's errors from telemetry's Loki, works out the facts, has an LLM write the
// report from them, and puts it at reports/<date>.md in the deploy bucket.
//
// It is read-only. Without an LLM key, or when the LLM fails, the report is the facts alone.
package reporter

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/duncan-G/protostrap/internal/aws"
	"github.com/duncan-G/protostrap/internal/infralog"
	"github.com/duncan-G/protostrap/internal/release"
	"github.com/duncan-G/protostrap/internal/secret"
	"github.com/duncan-G/protostrap/internal/spec"
)

// Config is what the reporter reads and where it writes.
type Config struct {
	App          string
	Environment  string
	DeployBucket string
	// Window is how far back the report looks. It defaults to a day.
	Window time.Duration
}

// Logs is telemetry's Loki, which holds the components' own logs (docs/telemetry.md).
type Logs interface {
	// Errors returns the lines from from to to that look like errors, each with its
	// component.
	Errors(ctx context.Context, from, to time.Time) ([]LogLine, error)
}

// LogLine is one line of a component's logs.
type LogLine struct {
	Time      time.Time
	Component string
	Message   string
}

// InfraLog is the infra log's archive in the deploy bucket.
type InfraLog interface {
	Read(ctx context.Context, from, to time.Time) ([]infralog.Record, error)
}

// Bucket is the deploy bucket.
type Bucket interface {
	Get(ctx context.Context, bucket, key string) (io.ReadCloser, error)
	Put(ctx context.Context, bucket, key string, body []byte) error
}

// Writer is an LLM.
type Writer interface {
	Write(ctx context.Context, system, user string) (Reply, error)
}

// Deps are the reporter's connections.
type Deps struct {
	Logs   Logs
	Infra  InfraLog
	Bucket Bucket
	// Secret reads the environment's secret, which holds Reporter_ANTHROPIC_API_KEY.
	Secret func(ctx context.Context) (string, error)
	LLM    func(key string) Writer
	Now    func() time.Time
	Logger *slog.Logger
}

// Run writes the report and returns its key.
func Run(ctx context.Context, cfg Config, d Deps) (string, error) {
	if cfg.Window == 0 {
		cfg.Window = 24 * time.Hour
	}
	if d.Now == nil {
		d.Now = time.Now
	}
	log := d.Logger
	if log == nil {
		log = slog.Default()
	}
	to := d.Now().UTC()
	from := to.Add(-cfg.Window)
	date := to.Format("2006-01-02")
	key := "reports/" + date + ".md"

	var notes []string
	rel, sp, err := running(ctx, d.Bucket, cfg.DeployBucket)
	switch {
	case errors.Is(err, aws.ErrNotFound):
		notes = append(notes, "No release has been deployed yet.")
	case err != nil:
		notes = append(notes, fmt.Sprintf("The running release can't be read: %v.", err))
	}

	records, err := d.Infra.Read(ctx, from, to)
	if err != nil {
		return "", fmt.Errorf("reading the infra log: %w", err)
	}
	// Without telemetry, the report is still written, and says what it lacks.
	errLines, err := d.Logs.Errors(ctx, from, to)
	unread := ""
	if err != nil {
		log.Warn("the components' errors couldn't be read from telemetry", "err", err)
		unread = err.Error()
	}
	facts := gather(records, errLines, sp, from, to)
	facts.ErrorsUnread = unread
	factsMD := facts.Markdown()

	var b strings.Builder
	fmt.Fprintf(&b, "# %s %s: %s\n\n", cfg.App, cfg.Environment, date)
	if rel.Commit != "" {
		fmt.Fprintf(&b, "Running release %s, ProtoStrap %s.\n\n", rel.Commit, rel.Version)
	}
	for _, n := range notes {
		fmt.Fprintf(&b, "%s\n\n", n)
	}

	apiKey, keyNote := llmKey(ctx, d.Secret)
	switch {
	case keyNote != "":
		fmt.Fprintf(&b, "_%s The report is the facts only._\n\n", keyNote)
	default:
		reply, err := d.LLM(apiKey).Write(ctx, systemPrompt, userPrompt(cfg, rel, factsMD))
		if err != nil {
			log.Warn("the LLM didn't write the report", "err", err)
			fmt.Fprintf(&b, "_The LLM didn't write a report: %v. The report is the facts only._\n\n", err)
			break
		}
		byline := "_Written by " + reply.Model
		if reply.FellBack {
			byline += ", a fallback model, after the configured model declined"
		}
		byline += ", from the facts below. Check anything important against them._\n\n"
		b.WriteString(byline)
		b.WriteString(strings.TrimSpace(reply.Text) + "\n\n")
		if reply.Truncated {
			b.WriteString("_The report was cut short at its token limit._\n\n")
		}
		log.Info("report written by the LLM", "model", reply.Model, "input_tokens", reply.InputTokens, "output_tokens", reply.OutputTokens)
	}
	b.WriteString("---\n\n")
	b.WriteString(factsMD)

	if err := d.Bucket.Put(ctx, cfg.DeployBucket, key, []byte(b.String())); err != nil {
		return "", fmt.Errorf("writing %s: %w", key, err)
	}
	return key, nil
}

// running is the release the scheduler is running, from state.json, and its spec.
func running(ctx context.Context, b Bucket, bucket string) (release.Release, *spec.Spec, error) {
	var st release.State
	if err := getJSON(ctx, b, bucket, "state.json", &st); err != nil {
		return release.Release{}, nil, err
	}
	commit := st.LastGood
	if st.Target != "" {
		commit = st.Target
	}
	if commit == "" {
		return release.Release{}, nil, aws.ErrNotFound
	}
	var rel release.Release
	if err := getJSON(ctx, b, bucket, "releases/"+commit+".json", &rel); err != nil {
		return release.Release{}, nil, err
	}
	sp, err := rel.ParseSpec()
	if err != nil {
		return rel, nil, err
	}
	return rel, sp, nil
}

func getJSON(ctx context.Context, b Bucket, bucket, key string, v any) error {
	body, err := b.Get(ctx, bucket, key)
	if err != nil {
		return err
	}
	defer body.Close()
	data, err := io.ReadAll(io.LimitReader(body, 16<<20))
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, v); err != nil {
		return fmt.Errorf("%s: %w", key, err)
	}
	return nil
}

// llmKey reads the key, or says why there is none.
func llmKey(ctx context.Context, read func(context.Context) (string, error)) (string, string) {
	const name = secret.Reporter + "ANTHROPIC_API_KEY"
	const unset = "No LLM key is set: an Org Admin sets " + name + " in the environment's secret."
	if read == nil {
		return "", unset
	}
	v, err := read(ctx)
	switch {
	case errors.Is(err, aws.ErrNotFound):
		return "", unset
	case err != nil:
		return "", fmt.Sprintf("The LLM key can't be read: %v.", err)
	}
	all, err := secret.Parse(v)
	if err != nil {
		return "", unset
	}
	key := strings.TrimSpace(all[name])
	if key == "" {
		key = strings.TrimSpace(all["ANTHROPIC_API_KEY"]) // It is the reporter's own secret, from before.
	}
	if key == "" {
		return "", unset
	}
	return key, ""
}

const systemPrompt = `You write the daily operations report for one environment of an application that runs on ProtoStrap.

ProtoStrap runs the application's containers on EC2 VMs in pools: ingress (the edge: cloudflared and Envoy), general, compute (Spot, with an On-Demand fallback) and data (stateful components such as PostgreSQL and Redis, one data VM each, never scaled). A scheduler on data-1 decides how many replicas each service and worker runs and where, and adds or removes VMs to fit them. A component's replicas follow desired = clamp(min(demand, limit), min, max), where demand is the highest of its signals (CPU as a share of its request, requests per second, queue depth, or a metric) and limits cap it whatever the signals say. Releases roll out a quarter of the replicas at a time and roll back on failure. All of this is set in the application's protostrap.yaml.

Readers are the application's developers. They use the report to decide whether to change protostrap.yaml or move a component to a managed service.

You get the facts the reporter extracted from the last day of logs and the protostrap.yaml that is running. Write markdown with exactly two sections:

## What happened
Releases, scaling, replicas that waited for a VM, limits that held a component back, lost VMs and errors. Lead with whatever needs attention. A quiet day takes two or three sentences.

## What to change
Concrete edits to protostrap.yaml, each naming the component or pool, the field, its current value, a suggested value and the evidence for it: components stuck at their maximum, CPU or memory requests too small or too large for what the component uses, data VMs running short, and components that meet a "Move when" below. If nothing needs changing, say so in one sentence.

Move when (from ProtoStrap's path to managed services):
- PostgreSQL to Amazon RDS: the data can't be lost or be offline for long, backups and upgrades take real effort, or the database needs a bigger data VM than is sensible.
- Redis to Amazon ElastiCache: Redis holds data that can't be rebuilt, its memory outgrows its share of the data VM, or its single command thread is saturated.
- VMs across availability zones: an outage in one zone taking the application offline is no longer acceptable. Only after the two above.
- Services and workers to ECS on Fargate: running VM pools costs more effort than Fargate's higher price per CPU.

Base every statement on the facts and the spec. Where they don't settle a question, say what to look at rather than guessing. The facts section is appended after your report, so refer to it instead of repeating its tables. Keep the report under 500 words.`

func userPrompt(cfg Config, rel release.Release, facts string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Application %s, environment %s.\n\n", cfg.App, cfg.Environment)
	if rel.Spec != "" {
		fmt.Fprintf(&b, "<spec release=%q>\n%s\n</spec>\n\n", rel.Commit, strings.TrimSpace(rel.Spec))
	} else {
		b.WriteString("No release is running, so there is no spec.\n\n")
	}
	fmt.Fprintf(&b, "<facts>\n%s\n</facts>\n\nWrite the report.", strings.TrimSpace(facts))
	return b.String()
}
