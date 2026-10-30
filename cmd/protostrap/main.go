// Command protostrap is ProtoStrap's one binary (docs/protostrap.md#components): the agent
// on every VM, the scheduler and the reporter on data-1, the edge and telemetry inside their
// containers, and the commands the workflows and operators run.
package main

import (
	"context"
	"crypto/tls"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"strconv"
	"syscall"
	"time"

	"github.com/duncan-G/protostrap/internal/agent"
	"github.com/duncan-G/protostrap/internal/aws"
	"github.com/duncan-G/protostrap/internal/credentials"
	"github.com/duncan-G/protostrap/internal/docker"
	"github.com/duncan-G/protostrap/internal/firewall"
	"github.com/duncan-G/protostrap/internal/host"
	"github.com/duncan-G/protostrap/internal/infralog"
	"github.com/duncan-G/protostrap/internal/ingress"
	"github.com/duncan-G/protostrap/internal/metrics"
	"github.com/duncan-G/protostrap/internal/probe"
	"github.com/duncan-G/protostrap/internal/schedclient"
	"github.com/duncan-G/protostrap/internal/systemd"
	"github.com/duncan-G/protostrap/internal/update"
)

// version is set at build time: -ldflags "-X main.version=1.4.0".
var version = "0.0.0-dev"

const usage = `usage: protostrap <command> [flags]

on VMs:
  agent       run the agent on this VM
  scheduler   run the scheduler (data-1)
  report      write the daily report (data-1)
  edge        run cloudflared and Envoy (the edge's container); edge render writes the local edge for aspire run
  telemetry   run the collector, Loki, Tempo, Prometheus and Grafana (the telemetry container)

on data-1, for operators and the deploy command:
  deploy      deploy a release: protostrap deploy <commit>
  status      show what runs where

in the workflows:
  check       check a spec
  plan        work out image tags, the images to build and the infra hash
  release     write a release

  version     print the version
`

func main() { os.Exit(run(os.Args, os.Stdout)) }

func run(args []string, out io.Writer) int {
	if len(args) < 2 {
		fmt.Fprint(out, usage)
		return 2
	}
	switch args[1] {
	case "agent":
		return runAgent(args[2:], out)
	case "scheduler":
		return runScheduler(args[2:], out)
	case "report":
		return runReport(args[2:], out)
	case "edge":
		return runEdge(args[2:], out)
	case "telemetry":
		return runTelemetry(args[2:], out)
	case "deploy":
		return runDeploy(args[2:], out)
	case "status":
		return runStatus(args[2:], out)
	case "check":
		return runCheck(args[2:], out)
	case "plan":
		return runPlan(args[2:], out)
	case "release":
		return runRelease(args[2:], out)
	case "version":
		fmt.Fprintln(out, version)
		return 0
	default:
		fmt.Fprint(out, usage)
		return 2
	}
}

func runAgent(args []string, out io.Writer) int {
	flags := flag.NewFlagSet("agent", flag.ContinueOnError)
	flags.SetOutput(out)
	path := flags.String("config", "/etc/protostrap/agent.json", "the agent's config file")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	// The agent's records go to the VM's journal and, with each sync, to the scheduler's
	// infra log (docs/protostrap.md#infra-log).
	outbox := &infralog.Outbox{}
	logger := slog.New(outbox.Handler(slog.NewJSONHandler(os.Stderr, nil)))
	cfg, err := loadConfig(*path)
	if err != nil {
		logger.Error("loading the config", "err", err)
		return 1
	}
	exe, err := os.Executable()
	if err != nil {
		logger.Error("finding the agent's binary", "err", err)
		return 1
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	err = startAgent(ctx, cfg, exe, logger, outbox)
	if errors.Is(err, agent.ErrUpdated) {
		logger.Info("restarting into the new version")
		err = syscall.Exec(exe, os.Args, os.Environ())
	}
	if err != nil {
		logger.Error("agent stopped", "err", err)
		return 1
	}
	return 0
}

// startAgent wires the agent to Docker, AWS, iptables and systemd, and runs it.
func startAgent(ctx context.Context, cfg config, exe string, logger *slog.Logger, outbox *infralog.Outbox) error {
	imds := &aws.IMDS{}
	id, err := imds.Identity(ctx)
	if err != nil {
		return fmt.Errorf("instance identity: %w", err)
	}
	vmCreds := &aws.CachedProvider{Source: imds, Window: 5 * time.Minute}
	region := id.Region

	s3 := &aws.S3{Region: region, Credentials: vmCreds}
	tlsConfig := &tls.Config{MinVersion: tls.VersionTLS12}
	switch {
	case cfg.SchedulerCAKey != "":
		// The scheduler publishes its certificate once data-1 is up, which may be after this
		// VM boots: fetch it when it is first needed, and again if the scheduler's certificate
		// changes.
		tlsConfig = schedclient.LazyTLSConfig(caSource{s3: s3, bucket: cfg.DeployBucket, key: cfg.SchedulerCAKey, file: cfg.SchedulerCA})
	case cfg.SchedulerCA != "":
		if tlsConfig, err = schedclient.TLSConfig(cfg.SchedulerCA); err != nil {
			return fmt.Errorf("scheduler CA: %w", err)
		}
	}
	sched, err := schedclient.New(cfg.Scheduler, tlsConfig, schedclient.Identity{Document: id.Document, Signature: id.Signature})
	if err != nil {
		return err
	}

	// This serves credentials for replicas at the port the firewall redirects 169.254.170.2 to.
	creds := credentials.NewServer(assumer{&aws.STS{Region: region, Credentials: vmCreds}}, credentials.Options{Logger: logger})
	listener, err := net.Listen("tcp", ":"+strconv.Itoa(cfg.CredentialsPort))
	if err != nil {
		return fmt.Errorf("credentials server: %w", err)
	}
	server := &http.Server{Handler: creds, ReadHeaderTimeout: 5 * time.Second}
	go server.Serve(listener)
	defer server.Close()

	helper := docker.CredentialHelper{Path: cfg.CredentialHelper}
	deps := agent.Deps{
		Runtime:     docker.New(cfg.DockerSocket, helper),
		Scheduler:   sched,
		Metadata:    metadata{imds},
		Firewall:    firewall.New(cfg.firewall(), firewall.Exec{}, firewall.ProcSysctl),
		Credentials: creds,
		Prober:      probe.New(2 * time.Second),
		Scraper:     metrics.NewScraper(3 * time.Second),
		Volumes:     host.Volumes{},
		Secrets:     componentSecrets{creds: creds, region: region},
		Checks: []agent.Check{
			{Name: "credential helper", Run: func(context.Context) error { return helper.Check() }},
			serviceActive(cfg.SSMService),
		},
		Updater:  &update.Updater{Store: bucket{s3, cfg.DeployBucket}, Executable: exe, Arch: runtime.GOARCH},
		Notifier: systemd.FromEnv(),
		Logger:   logger,
		Logs:     outbox,
	}
	if cfg.AutoScalingGroup != "" {
		deps.Hooks = hooks{as: &aws.AutoScaling{Region: region, Credentials: vmCreds},
			group: cfg.AutoScalingGroup, launch: cfg.LaunchHook, terminate: cfg.TerminateHook, instance: id.InstanceID}
	}
	if cfg.BackupsBucket != "" {
		deps.Backups = bucket{s3, cfg.BackupsBucket}
	}
	if cfg.Ingress != nil {
		deps.Zone = &ingress.Takeover{EC2: &aws.EC2{Region: region, Credentials: vmCreds}, InstanceID: id.InstanceID,
			AllocationID: cfg.Ingress.ElasticIP, RouteTables: cfg.Ingress.RouteTables, Logger: logger}
	}

	a, err := agent.New(agent.Config{
		App: cfg.App, InstanceID: id.InstanceID, Name: cfg.Name, Pool: cfg.Pool, Zone: id.Zone,
		Region: region, Version: version, PrivateIP: id.PrivateIP, SwarmManager: cfg.SwarmManager,
		HookTimeout: cfg.TerminateHookTimeout.Duration, BackupHour: cfg.BackupHour,
		TelemetryAddress: cfg.telemetryAddress(),
	}, deps)
	if err != nil {
		return err
	}
	return a.Run(ctx)
}
