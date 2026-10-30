package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/duncan-G/protostrap/internal/aws"
	"github.com/duncan-G/protostrap/internal/release"
	"github.com/duncan-G/protostrap/internal/scheduler"
	"github.com/duncan-G/protostrap/internal/update"
)

// localScheduler is the scheduler's API on data-1, reached over loopback.
type localScheduler struct {
	base string
	http *http.Client
}

func newLocalScheduler(cfg schedulerConfig) (*localScheduler, error) {
	pemBytes, err := os.ReadFile(filepath.Join(cfg.StateDir, "cert.pem"))
	if err != nil {
		return nil, fmt.Errorf("the scheduler's certificate: %w (is the scheduler running on this VM?)", err)
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(pemBytes)
	tr := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}, Proxy: nil}
	return &localScheduler{base: "https://" + cfg.localAPI(), http: &http.Client{Transport: tr, Timeout: 15 * time.Second}}, nil
}

func (l *localScheduler) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		b, _ := json.Marshal(in)
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, l.base+path, body)
	if err != nil {
		return err
	}
	resp, err := l.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if resp.StatusCode != http.StatusOK {
		var e struct{ Error string }
		json.Unmarshal(data, &e)
		if resp.StatusCode == http.StatusNotFound {
			return fmt.Errorf("%s: %w", e.Error, scheduler.ErrNotFound)
		}
		return fmt.Errorf("scheduler %s: HTTP %d: %s", path, resp.StatusCode, e.Error)
	}
	return json.Unmarshal(data, out)
}

func (l *localScheduler) Status(ctx context.Context) (scheduler.Status, error) {
	var st scheduler.Status
	err := l.do(ctx, http.MethodGet, "/v1/status", nil, &st)
	return st, err
}

func (l *localScheduler) Deploy(ctx context.Context, commit string) error {
	var out map[string]string
	return l.do(ctx, http.MethodPost, "/v1/releases", map[string]string{"commit": commit}, &out)
}

// deployer is `protostrap deploy <commit>`, which the protostrap-deploy SSM command runs on
// data-1 (docs/protostrap.md#deploy-a-release): it updates the scheduler first if the
// release names another ProtoStrap version, makes the release the target, and follows the
// rollout until it is done, failed and rolled back, or superseded.
type deployer struct {
	api interface {
		Status(ctx context.Context) (scheduler.Status, error)
		Deploy(ctx context.Context, commit string) error
	}
	release func(ctx context.Context, commit string) (release.Release, error)
	update  func(ctx context.Context, version string) error // It installs a version's binary.
	restart func(ctx context.Context) error                 // It restarts the scheduler.
	report  func()                                          // It starts the reporter.
	out     io.Writer
	poll    time.Duration
	timeout time.Duration
	clock   func() time.Time
	sleep   func(ctx context.Context, d time.Duration) error
}

func (d deployer) run(ctx context.Context, commit string) error {
	rel, err := d.release(ctx, commit)
	if err != nil {
		return fmt.Errorf("reading the release: %w", err)
	}
	st, err := d.api.Status(ctx)
	if err != nil {
		return fmt.Errorf("the scheduler doesn't answer: %w", err)
	}
	if st.Version != rel.Version {
		fmt.Fprintf(d.out, "updating the scheduler from ProtoStrap %s to %s\n", st.Version, rel.Version)
		if err := d.update(ctx, rel.Version); err != nil {
			return fmt.Errorf("updating the scheduler: %w", err)
		}
		if err := d.restart(ctx); err != nil {
			return fmt.Errorf("restarting the scheduler: %w", err)
		}
		deadline := d.clock().Add(3 * time.Minute)
		for {
			if st, err = d.api.Status(ctx); err == nil && st.Version == rel.Version {
				break
			}
			if d.clock().After(deadline) {
				return fmt.Errorf("the scheduler didn't come back on %s", rel.Version)
			}
			if err := d.sleep(ctx, d.poll); err != nil {
				return err
			}
		}
	}
	// A scheduler that has just started is warming up for a few seconds.
	deadline := d.clock().Add(time.Minute)
	for {
		err = d.api.Deploy(ctx, commit)
		if err == nil || errors.Is(err, scheduler.ErrNotFound) || d.clock().After(deadline) {
			break
		}
		if err := d.sleep(ctx, d.poll); err != nil {
			return err
		}
	}
	if err != nil {
		return fmt.Errorf("deploying %s: %w", commit, err)
	}
	fmt.Fprintf(d.out, "deploying %s (%s, ProtoStrap %s)\n", commit, rel.App, rel.Version)

	end := d.clock().Add(d.timeout)
	last := ""
	for {
		st, err := d.api.Status(ctx)
		if err == nil {
			if line := progress(st); line != last {
				fmt.Fprintln(d.out, line)
				last = line
			}
			for _, r := range st.Rollouts {
				if r.Commit != commit {
					continue
				}
				switch r.State {
				case scheduler.RolloutDone:
					fmt.Fprintf(d.out, "done: %s runs\n", commit)
					if d.report != nil {
						d.report()
					}
					return nil
				case scheduler.RolloutFailed:
					return fmt.Errorf("failed and rolled back to %q: %s", st.Target, r.Reason)
				case scheduler.RolloutSuperseded:
					return fmt.Errorf("superseded: %s", r.Reason)
				}
			}
		}
		if d.clock().After(end) {
			return fmt.Errorf("still rolling out after %v: see `protostrap status` on data-1", d.timeout)
		}
		if err := d.sleep(ctx, d.poll); err != nil {
			return err
		}
	}
}

// progress is one line on how far a rollout has got.
func progress(st scheduler.Status) string {
	var parts []string
	for _, c := range st.Components {
		if c.Kind == "task" {
			continue
		}
		part := fmt.Sprintf("%s %d/%d", c.Name, c.Ready, c.Desired)
		if c.Old > 0 {
			part += fmt.Sprintf(" (%d old)", c.Old)
		}
		if c.Pending > 0 {
			part += fmt.Sprintf(" (%d waiting for room)", c.Pending)
		}
		parts = append(parts, part)
	}
	return "  " + strings.Join(parts, ", ")
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func runDeploy(args []string, out io.Writer) int {
	flags := flag.NewFlagSet("deploy", flag.ContinueOnError)
	flags.SetOutput(out)
	path := flags.String("config", "/etc/protostrap/scheduler.json", "the scheduler's config file")
	timeout := flags.Duration("timeout", 80*time.Minute, "how long to follow the rollout")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if flags.NArg() != 1 || !release.ValidCommit(flags.Arg(0)) {
		fmt.Fprintln(out, "usage: protostrap deploy [--config file] <commit>")
		return 2
	}
	commit := flags.Arg(0)
	cfg, err := loadSchedulerConfig(*path)
	if err != nil {
		fmt.Fprintln(out, err)
		return 1
	}
	api, err := newLocalScheduler(cfg)
	if err != nil {
		fmt.Fprintln(out, err)
		return 1
	}
	creds := &aws.CachedProvider{Source: &aws.IMDS{}, Window: 5 * time.Minute}
	s3 := &aws.S3{Region: cfg.Region, Credentials: creds}
	exe := "/usr/local/bin/protostrap"
	d := deployer{
		api: api,
		release: func(ctx context.Context, commit string) (release.Release, error) {
			return deployStore{s3, cfg.DeployBucket}.Release(ctx, commit)
		},
		update: func(ctx context.Context, v string) error {
			return (&update.Updater{Store: bucket{s3, cfg.DeployBucket}, Executable: exe, Arch: runtime.GOARCH}).Update(ctx, v)
		},
		restart: func(ctx context.Context) error {
			return exec.CommandContext(ctx, "systemctl", "restart", "protostrap-scheduler.service").Run()
		},
		report: func() { exec.Command("systemctl", "start", "--no-block", "protostrap-report.service").Run() },
		out:    out, poll: 5 * time.Second, timeout: *timeout, clock: time.Now, sleep: sleepCtx,
	}
	if err := d.run(context.Background(), commit); err != nil {
		fmt.Fprintln(out, "protostrap deploy:", err)
		return 1
	}
	return 0
}

// runStatus is `protostrap status`, which shows what runs where, on data-1.
func runStatus(args []string, out io.Writer) int {
	flags := flag.NewFlagSet("status", flag.ContinueOnError)
	flags.SetOutput(out)
	path := flags.String("config", "/etc/protostrap/scheduler.json", "the scheduler's config file")
	asJSON := flags.Bool("json", false, "print JSON")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	cfg, err := loadSchedulerConfig(*path)
	if err != nil {
		fmt.Fprintln(out, err)
		return 1
	}
	api, err := newLocalScheduler(cfg)
	if err != nil {
		fmt.Fprintln(out, err)
		return 1
	}
	st, err := api.Status(context.Background())
	if err != nil {
		fmt.Fprintln(out, err)
		return 1
	}
	if *asJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		enc.Encode(st)
		return 0
	}
	printStatus(out, st)
	return 0
}

func printStatus(out io.Writer, st scheduler.Status) {
	fmt.Fprintf(out, "ProtoStrap %s, release %s (last good %s)\n", st.Version, orNone(st.Target), orNone(st.LastGood))
	if n := len(st.Rollouts); n > 0 {
		r := st.Rollouts[n-1]
		fmt.Fprintf(out, "last rollout: %s %s %s\n", r.Commit, r.State, r.Reason)
	}
	w := tabwriter.NewWriter(out, 0, 4, 2, ' ', 0)
	fmt.Fprintln(w, "\nPOOL\tVMS\tBOOTING\tWANTED\tREQUESTED CPU")
	for _, p := range st.Pools {
		fmt.Fprintf(w, "%s\t%d\t%d\t%d\t%dm\n", p.Name, p.VMs, p.Booting, p.Wanted, p.Requested.CPU)
	}
	fmt.Fprintln(w, "\nCOMPONENT\tDESIRED\tREADY\tSTARTING\tWAITING\tOLD")
	for _, c := range st.Components {
		fmt.Fprintf(w, "%s\t%d\t%d\t%d\t%d\t%d\n", c.Name, c.Desired, c.Ready, c.Starting, c.Pending, c.Old)
	}
	fmt.Fprintln(w, "\nVM\tPOOL\tNAME\tFREE CPU\tREPLICAS\tSTATE")
	for _, v := range st.VMs {
		state := "ok"
		switch {
		case v.Lost:
			state = "silent"
		case v.Terminating:
			state = "terminating"
		case v.Cordoned:
			state = "draining"
		}
		fmt.Fprintf(w, "%s\t%s\t%s\t%dm\t%d\t%s\n", v.ID, v.Pool, v.Name, v.Free.CPU, v.Replicas, state)
	}
	w.Flush()
	if sec := st.Secret; sec != nil {
		if sec.Error != "" {
			fmt.Fprintf(out, "\nsecret %s: can't be read: %s\n", sec.Name, sec.Error)
		} else {
			fmt.Fprintf(out, "\nsecret %s: version %s\n", sec.Name, orNone(sec.Version))
		}
	}
	for _, c := range st.Components {
		switch k := c.Secret; {
		case k == nil:
		case k.Failed != "":
			fmt.Fprintf(out, "keys of %s: %s fails: %s\n", c.Name, k.Version, k.Failed)
		case k.Pending != "":
			fmt.Fprintf(out, "keys of %s: %s waits: %s\n", c.Name, k.Pending, k.Waiting)
		}
		switch f := c.Config; {
		case f == nil:
		case f.Failed != "":
			fmt.Fprintf(out, "config of %s: %s fails: %s\n", c.Name, f.Digest, f.Failed)
		case f.Behind > 0:
			fmt.Fprintf(out, "config of %s: %s, %d replicas still to take it\n", c.Name, f.Digest, f.Behind)
		}
		switch k := c.Configure; {
		case k == nil || k.State == "done":
		case k.Failed != "":
			fmt.Fprintf(out, "configure %s with %s: failed: %s\n", c.Name, k.Task, strings.TrimSuffix(k.Failed+". "+k.Waiting, ". "))
		default:
			fmt.Fprintf(out, "configure %s with %s: %s %s\n", c.Name, k.Task, k.State, k.Waiting)
		}
	}
	var failed []string
	for _, r := range st.Replicas {
		if r.State == "failed" {
			failed = append(failed, r.ID+": "+r.Reason)
		}
	}
	slices.Sort(failed)
	for _, f := range failed {
		fmt.Fprintln(out, "failed:", f)
	}
}

func orNone(s string) string {
	if s == "" {
		return "none"
	}
	return s
}
