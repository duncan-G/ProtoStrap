package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"

	"github.com/duncan-G/protostrap/internal/agent"
	"github.com/duncan-G/protostrap/internal/aws"
	"github.com/duncan-G/protostrap/internal/credentials"
)

// metadata finds the VM's removal in instance metadata: a Spot interruption notice, or
// the Auto Scaling group moving it to Terminated, which its terminate hook holds.
type metadata struct{ imds *aws.IMDS }

func (m metadata) Termination(ctx context.Context) (*agent.Notice, error) {
	at, spot, err := m.imds.SpotInterruption(ctx)
	if err != nil {
		return nil, err
	}
	if spot {
		return &agent.Notice{Spot: true, At: at}, nil
	}
	state, err := m.imds.TargetLifecycleState(ctx)
	if err != nil {
		return nil, err
	}
	if state == "Terminated" {
		return &agent.Notice{}, nil
	}
	return nil, nil
}

// hooks completes the VM's lifecycle hooks in its Auto Scaling group.
type hooks struct {
	as                *aws.AutoScaling
	group             string
	launch, terminate string
	instance          string
}

func (h hooks) name(hook agent.Hook) string {
	if hook == agent.LaunchHook {
		return h.launch
	}
	return h.terminate
}

// Complete completes the hook. Auto Scaling answers a hook that isn't in progress, because
// it was completed or timed out, with a ValidationError, which it wraps in ErrHookDone.
func (h hooks) Complete(ctx context.Context, hook agent.Hook, result string) error {
	err := h.as.CompleteLifecycleAction(ctx, h.group, h.name(hook), h.instance, result)
	var apiErr *aws.APIError
	if errors.As(err, &apiErr) && apiErr.Code == "ValidationError" && strings.Contains(apiErr.Message, "No active Lifecycle Action") {
		return fmt.Errorf("%w: %w", agent.ErrHookDone, err)
	}
	return err
}

func (h hooks) Heartbeat(ctx context.Context, hook agent.Hook) error {
	return h.as.RecordLifecycleActionHeartbeat(ctx, h.group, h.name(hook), h.instance)
}

// assumer takes on component roles with the VM's role. Chained roles last an hour at most.
type assumer struct{ sts *aws.STS }

func (a assumer) AssumeRole(ctx context.Context, role, session string) (aws.Credentials, error) {
	return a.sts.AssumeRole(ctx, role, session, time.Hour)
}

// componentSecrets reads the environment's secret for the agent's secret files, with the
// component's role, whose credentials it shares with the component's replicas.
type componentSecrets struct {
	creds    *credentials.Server
	region   string
	endpoint string // It is for tests.
}

func (s componentSecrets) Read(ctx context.Context, secretID, version, role, component string) (string, string, error) {
	sm := &aws.SecretsManager{Endpoint: s.endpoint, Region: s.region, Credentials: roleCredentials{s.creds, role, component}}
	return sm.GetSecretValueVersion(ctx, secretID, version)
}

type roleCredentials struct {
	creds           *credentials.Server
	role, component string
}

func (r roleCredentials) Retrieve(ctx context.Context) (aws.Credentials, error) {
	return r.creds.RoleCredentials(ctx, r.role, r.component)
}

// bucket is one S3 bucket: the deploy bucket for updates, the backups bucket for backups.
type bucket struct {
	s3   *aws.S3
	name string
}

func (b bucket) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	return b.s3.Get(ctx, b.name, key)
}

func (b bucket) Exists(ctx context.Context, key string) (bool, error) {
	return b.s3.Exists(ctx, b.name, key)
}

func (b bucket) Upload(ctx context.Context, key string, body io.Reader) error {
	return b.s3.Upload(ctx, b.name, key, body)
}

type realClock = agent.SystemClock

// serviceActive checks a systemd service is running, such as the SSM Agent.
func serviceActive(name string) agent.Check {
	return agent.Check{Name: name, Run: func(ctx context.Context) error {
		return exec.CommandContext(ctx, "systemctl", "is-active", "--quiet", name).Run()
	}}
}
