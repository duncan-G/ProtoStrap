package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/duncan-G/protostrap/internal/aws"
	"github.com/duncan-G/protostrap/internal/infralog"
	"github.com/duncan-G/protostrap/internal/reporter"
	"github.com/duncan-G/protostrap/internal/secret"
	"github.com/duncan-G/protostrap/internal/telemetry"
)

// runReport writes the daily report on data-1, from the scheduler's config. The systemd
// timer runs it once a day, and `protostrap deploy` after every release.
func runReport(args []string, out io.Writer) int {
	flags := flag.NewFlagSet("report", flag.ContinueOnError)
	flags.SetOutput(out)
	path := flags.String("config", "/etc/protostrap/scheduler.json", "the scheduler's config file")
	toStdout := flags.Bool("print", false, "print the report instead of writing it to the deploy bucket")
	window := flags.Duration("window", 24*time.Hour, "how far back to read")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	cfg, err := loadSchedulerConfig(*path)
	if err != nil {
		log.Error("loading the config", "err", err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	ctx, cancel := context.WithTimeout(ctx, 25*time.Minute)
	defer cancel()

	creds := &aws.CachedProvider{Source: &aws.IMDS{}, Window: 5 * time.Minute}
	var bucket reporter.Bucket = &aws.S3{Region: cfg.Region, Credentials: creds}
	if *toStdout {
		bucket = printBucket{bucket, out}
	}
	model := cfg.ReportModel
	if model == "" {
		model = reporter.DefaultModel
	}
	secrets := &aws.SecretsManager{Region: cfg.Region, Credentials: creds}
	key, err := reporter.Run(ctx, reporter.Config{App: cfg.App, Environment: cfg.Environment, DeployBucket: cfg.DeployBucket, Window: *window}, reporter.Deps{
		Logs:   &reporter.Loki{Client: telemetry.SocketClient(telemetry.GrafanaSocket), Base: "http://grafana"},
		Infra:  &infralog.Archive{Bucket: &aws.S3{Region: cfg.Region, Credentials: creds}, Name: cfg.DeployBucket},
		Bucket: bucket,
		Secret: func(ctx context.Context) (string, error) {
			// The key is in the environment's secret; a config without an environment names
			// the reporter's own.
			name := cfg.ReportSecret
			if cfg.Environment != "" {
				name = secret.Name(cfg.App, cfg.Environment)
			}
			if name == "" {
				return "", aws.ErrNotFound
			}
			return secrets.GetSecretValue(ctx, name)
		},
		LLM:    func(apiKey string) reporter.Writer { return &reporter.Claude{Key: apiKey, Model: model} },
		Logger: log,
	})
	if err != nil {
		log.Error("report", "err", err)
		return 1
	}
	if !*toStdout {
		log.Info("report written", "bucket", cfg.DeployBucket, "key", key)
	}
	return 0
}

// printBucket reads from the deploy bucket and prints what would be written to it.
type printBucket struct {
	reporter.Bucket
	out io.Writer
}

func (p printBucket) Put(_ context.Context, _, _ string, body []byte) error {
	_, err := fmt.Fprint(p.out, string(body))
	return err
}
