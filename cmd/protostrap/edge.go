package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/duncan-G/protostrap/internal/aws"
	"github.com/duncan-G/protostrap/internal/edge"
)

// runEdge is `protostrap edge`, the edge container's entrypoint (edge/Dockerfile): it runs
// Envoy and cloudflared side by side (docs/protostrap.md#edge).
func runEdge(args []string, out io.Writer) int {
	if len(args) > 0 && args[0] == "render" {
		return runEdgeRender(args[1:], out)
	}
	flags := flag.NewFlagSet("edge", flag.ContinueOnError)
	flags.SetOutput(out)
	files := flags.String("files", "/etc/protostrap", "where the agent writes Envoy's listener and clusters")
	envoy := flags.String("envoy", "/usr/local/bin/envoy", "Envoy's binary")
	cloudflared := flags.String("cloudflared", "/usr/local/bin/cloudflared", "cloudflared's binary")
	statusAddr := flags.String("status", ":9902", "where the edge answers its readiness check and serves its metrics")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	region, secret := os.Getenv("AWS_REGION"), os.Getenv("SECRET_ID")
	if region == "" || secret == "" {
		fmt.Fprintln(out, "protostrap edge: AWS_REGION and SECRET_ID must be set, as the agent does")
		return 2
	}
	sm := &aws.SecretsManager{Region: region, Credentials: &aws.CachedProvider{Source: &aws.ContainerCredentials{}, Window: 5 * time.Minute}}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	err := edge.Run(ctx, edge.Config{
		Envoy: *envoy, Cloudflared: *cloudflared, FilesDir: *files, BootstrapPath: "/tmp/envoy.json",
		StatusAddr: *statusAddr, Logger: log,
		Token: edge.TokenFrom(func(ctx context.Context) (string, error) { return sm.GetSecretValue(ctx, secret) }, os.Getenv("SECRET_PREFIX")),
	})
	if err != nil {
		log.Error("edge stopped", "err", err)
		return 1
	}
	return 0
}

// runEdgeRender is `protostrap edge render`: it writes the edge `aspire run` runs, Envoy's
// bootstrap, listener and clusters, from the routing ProtoStrap.Hosting writes, so the same
// gateway runs locally as in an account (docs/protostrap-aspire.md, "The local edge").
func runEdgeRender(args []string, out io.Writer) int {
	flags := flag.NewFlagSet("edge render", flag.ContinueOnError)
	flags.SetOutput(out)
	routingPath := flags.String("routing", "", "the routing, as JSON: services, hosts and where Envoy listens")
	dir := flags.String("out", "", "where to write envoy.json, lds.json and cds.json")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if *routingPath == "" || *dir == "" {
		fmt.Fprintln(out, "protostrap edge render: --routing and --out are required")
		return 2
	}
	b, err := os.ReadFile(*routingPath)
	if err != nil {
		fmt.Fprintf(out, "protostrap edge render: %v\n", err)
		return 1
	}
	var routing edge.Routing
	if err := json.Unmarshal(b, &routing); err != nil {
		fmt.Fprintf(out, "protostrap edge render: %s: %v\n", *routingPath, err)
		return 1
	}
	if err := edge.WriteLocal(*dir, routing); err != nil {
		fmt.Fprintf(out, "protostrap edge render: %v\n", err)
		return 1
	}
	return 0
}
