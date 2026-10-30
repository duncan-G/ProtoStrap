// Command load drives the sample application through the gateway, to watch ProtoStrap
// scale it.
//
//	go run ./sample/load http  -url https://HOST/py/hello -rps 50 -duration 2m [-concurrency 64] [-ramp 30s]
//	go run ./sample/load steps -url https://HOST/py/hello -steps 10,50,100,20 -step 3m
//	go run ./sample/load jobs  -url https://HOST/api/jobs -count 1000 -work-ms 1000
//
// http and steps send requests at a fixed rate whatever the answers (an open loop), and
// print what came back every 10 seconds. jobs puts jobs on the queue through the api.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const usage = `usage:
  load http  -url URL -rps N -duration D [-concurrency N] [-ramp D] [-timeout D] [-h2c]
  load steps -url URL -steps N,N,... -step D [-concurrency N] [-ramp D] [-timeout D] [-h2c]
  load jobs  -url URL -count N [-work-ms N] [-batch N] [-h2c]
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var err error
	switch cmd, args := os.Args[1], os.Args[2:]; cmd {
	case "http":
		err = runHTTP(ctx, args)
	case "steps":
		err = runSteps(ctx, args)
	case "jobs":
		err = runJobs(ctx, args)
	default:
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "load:", err)
		os.Exit(1)
	}
}

// rateFlags are the flags http and steps share.
type rateFlags struct {
	url         string
	concurrency int
	ramp        time.Duration
	timeout     time.Duration
	h2c         bool
}

func (f *rateFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&f.url, "url", "", "URL to GET")
	fs.IntVar(&f.concurrency, "concurrency", 64, "most requests in flight; a send beyond it is dropped, not delayed")
	fs.DurationVar(&f.ramp, "ramp", 0, "time to move linearly to each new rate")
	fs.DurationVar(&f.timeout, "timeout", 10*time.Second, "time allowed for each request")
	fs.BoolVar(&f.h2c, "h2c", false, h2cUsage)
}

func (f *rateFlags) check() error {
	if f.url == "" {
		return errors.New("-url is required")
	}
	if f.concurrency < 1 {
		return errors.New("-concurrency must be at least 1")
	}
	return nil
}

func runHTTP(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("http", flag.ExitOnError)
	var f rateFlags
	f.register(fs)
	rps := fs.Float64("rps", 10, "requests per second")
	duration := fs.Duration("duration", time.Minute, "how long to send for")
	fs.Parse(args)
	if err := f.check(); err != nil {
		return err
	}
	r := newRunner(f)
	r.run(ctx, []phase{{rps: *rps, duration: *duration}})
	return nil
}

func runSteps(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("steps", flag.ExitOnError)
	var f rateFlags
	f.register(fs)
	steps := fs.String("steps", "", "comma-separated rates, in requests per second, e.g. 10,50,100,20")
	step := fs.Duration("step", 3*time.Minute, "how long each rate lasts")
	fs.Parse(args)
	if err := f.check(); err != nil {
		return err
	}
	var phases []phase
	for s := range strings.SplitSeq(*steps, ",") {
		rps, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
		if err != nil || rps < 0 {
			return fmt.Errorf("bad rate %q in -steps", s)
		}
		phases = append(phases, phase{rps: rps, duration: *step})
	}
	if len(phases) == 0 || *steps == "" {
		return errors.New("-steps is required")
	}
	r := newRunner(f)
	r.run(ctx, phases)
	return nil
}

func runJobs(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("jobs", flag.ExitOnError)
	target := fs.String("url", "", "the api's jobs endpoint, e.g. https://HOST/api/jobs")
	count := fs.Int("count", 100, "jobs to enqueue")
	workMs := fs.Int("work-ms", 1000, "how long each job keeps a worker CPU busy, in ms (at most 10000)")
	batch := fs.Int("batch", 1000, "jobs per POST (at most 1000)")
	h2c := fs.Bool("h2c", false, h2cUsage)
	fs.Parse(args)
	if *target == "" {
		return errors.New("-url is required")
	}
	u, err := url.Parse(*target)
	if err != nil {
		return err
	}
	size := min(max(*batch, 1), 1000)
	client := &http.Client{Transport: newTransport(*h2c), Timeout: 2 * time.Minute}
	start := time.Now()
	requested, sent := 0, 0
	for requested < *count {
		n := min(size, *count-requested)
		q := u.Query()
		q.Set("count", strconv.Itoa(n))
		q.Set("work_ms", strconv.Itoa(*workMs))
		u.RawQuery = q.Encode()
		got, err := postJobs(ctx, client, u.String())
		if err != nil {
			return fmt.Errorf("after %d jobs sent: %w", sent, err)
		}
		requested += n
		sent += got
		fmt.Printf("%6s  sent %d/%d\n", since(start), sent, *count)
	}
	if sent < *count {
		return fmt.Errorf("SQS took %d of %d jobs", sent, *count)
	}
	return nil
}

func postJobs(ctx context.Context, client *http.Client, target string) (int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, target, nil)
	if err != nil {
		return 0, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil {
		return 0, err
	}
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("%s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	var reply struct {
		Sent int `json:"sent"`
	}
	if err := json.Unmarshal(body, &reply); err != nil {
		return 0, fmt.Errorf("reading the reply: %w", err)
	}
	return reply.Sent, nil
}

const h2cUsage = "speak cleartext HTTP/2 to an http:// URL, as the api needs when called without the gateway"

// newTransport returns a copy of the default transport; with h2c, it uses cleartext HTTP/2
// for http:// URLs (and HTTP/2 over TLS for https:// ones).
func newTransport(h2c bool) *http.Transport {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if h2c {
		protocols := new(http.Protocols)
		protocols.SetUnencryptedHTTP2(true)
		protocols.SetHTTP2(true)
		transport.Protocols = protocols
	}
	return transport
}

func since(start time.Time) string {
	return time.Since(start).Round(time.Second).String()
}
