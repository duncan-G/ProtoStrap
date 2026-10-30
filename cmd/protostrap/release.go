package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/duncan-G/protostrap/internal/release"
	"github.com/duncan-G/protostrap/internal/spec"
)

// runCheck is `protostrap check`, which checks a spec against its rules
// (docs/protostrap.md#spec).
func runCheck(args []string, out io.Writer) int {
	flags := flag.NewFlagSet("check", flag.ContinueOnError)
	flags.SetOutput(out)
	path := flags.String("spec", "protostrap.yaml", "the application's spec")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	s, err := spec.ReadFile(*path)
	if err != nil {
		fmt.Fprintln(out, err)
		return 1
	}
	problems := s.Check()
	for _, p := range problems {
		fmt.Fprintf(out, "%s: %s\n", *path, p)
	}
	if len(problems) > 0 {
		fmt.Fprintf(out, "%s: %d problems\n", *path, len(problems))
		return 1
	}
	fmt.Fprintf(out, "%s: ok\n", *path)
	return 0
}

// runPlan is `protostrap plan`, which works out every built component's image tag, the
// images to copy, and the infra projection's hash (docs/protostrap-aspire.md#protostrap-plan).
func runPlan(args []string, out io.Writer) int {
	flags := flag.NewFlagSet("plan", flag.ContinueOnError)
	flags.SetOutput(out)
	path := flags.String("spec", "protostrap.yaml", "the application's spec")
	registry := flags.String("registry", "registry.invalid", "the ECR registry images go to")
	ver := flags.String("version", "", "the ProtoStrap version the release runs; default the spec's")
	asJSON := flags.Bool("json", false, "print JSON")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	p, err := release.MakePlan(*path, *registry, *ver)
	if err != nil {
		fmt.Fprintln(out, err)
		return 1
	}
	if *asJSON {
		enc := json.NewEncoder(out)
		enc.SetIndent("", "  ")
		enc.Encode(p)
		return 0
	}
	fmt.Fprintf(out, "%s on ProtoStrap %s, infra %s\n", p.App, p.Version, p.InfraHash)
	for _, b := range p.Builds {
		fmt.Fprintf(out, "  %-20s %s  from %s\n", b.Component, b.Tag, strings.Join(b.Sources, ", "))
	}
	for _, st := range p.Stateful {
		fmt.Fprintf(out, "  %-20s %s\n", st.Component, st.Upstream)
	}
	return 0
}

// runRelease is `protostrap release`, which writes releases/<commit>.json for the deploy
// workflow to upload.
func runRelease(args []string, out io.Writer) int {
	flags := flag.NewFlagSet("release", flag.ContinueOnError)
	flags.SetOutput(out)
	path := flags.String("spec", "protostrap.yaml", "the application's spec")
	registry := flags.String("registry", "", "the ECR registry images are in")
	commit := flags.String("commit", "", "the commit being released")
	ver := flags.String("version", "", "the ProtoStrap version the release runs; default the spec's")
	dest := flags.String("out", "", "where to write it; default stdout")
	if err := flags.Parse(args); err != nil {
		return 2
	}
	if *registry == "" || *commit == "" {
		fmt.Fprintln(out, "release: --registry and --commit are required")
		return 2
	}
	r, err := release.Make(*path, *commit, *registry, *ver)
	if err != nil {
		fmt.Fprintln(out, err)
		return 1
	}
	b, _ := json.MarshalIndent(r, "", "  ")
	b = append(b, '\n')
	if *dest == "" {
		out.Write(b)
		return 0
	}
	if err := os.WriteFile(*dest, b, 0o644); err != nil {
		fmt.Fprintln(out, err)
		return 1
	}
	fmt.Fprintf(out, "release %s written to %s\n", r.Commit, *dest)
	return 0
}
