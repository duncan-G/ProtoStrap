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

	"github.com/duncan-G/protostrap/internal/telemetry"
)

// runTelemetry is `protostrap telemetry`, the telemetry container's entrypoint
// (telemetry/Dockerfile): it runs the collector, Loki, Tempo, Prometheus and Grafana side by
// side (docs/telemetry.md).
func runTelemetry(args []string, out io.Writer) int {
	flags := flag.NewFlagSet("telemetry", flag.ContinueOnError)
	flags.SetOutput(out)
	data := flags.String("data", telemetry.VolumePath, "the telemetry volume")
	files := flags.String("files", "/etc/protostrap", "where the agent writes the scrape targets")
	socket := flags.String("socket", telemetry.SocketDir, "where Grafana's socket goes")
	config := flags.String("config", "/tmp/protostrap-telemetry", "where the programs' configs are written")
	cgroup := flags.String("cgroup-memory", "/sys/fs/cgroup/memory.max", "the container's memory limit, which the programs share")
	uid := flags.Int("uid", 10001, "the user the programs run as, or -1 for this one")
	gid := flags.Int("gid", 10001, "the group the programs run as")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	c, err := telemetry.EnvConfig(os.Getenv)
	if err != nil {
		fmt.Fprintf(out, "protostrap telemetry: %v: the scheduler sets these\n", err)
		return 2
	}
	if c.MemoryMiB, err = telemetry.CgroupMemoryMiB(*cgroup); err != nil {
		log.Warn("the container's memory limit is unknown: the programs share the default", "err", err)
	}
	c.DataDir, c.FilesDir, c.SocketDir, c.ConfigDir = *data, *files, *socket, *config
	c.Bin, c.UID, c.GID, c.Logger = telemetry.DefaultBinaries, *uid, *gid, log
	if os.Geteuid() != 0 {
		c.UID, c.GID = -1, -1 // Only root can give the programs a user of their own.
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	if err := telemetry.Run(ctx, c); err != nil {
		log.Error("telemetry stopped", "err", err)
		return 1
	}
	return 0
}
