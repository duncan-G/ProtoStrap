package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"time"

	"github.com/duncan-G/protostrap/internal/firewall"
	"github.com/duncan-G/protostrap/internal/telemetry"
)

// config is /etc/protostrap/agent.json, which the VM's user data writes. What AWS
// already knows, such as the instance, its zone and region, comes from instance metadata.
type config struct {
	App          string `json:"app"`
	Pool         string `json:"pool"`
	Name         string `json:"name"`
	SwarmManager bool   `json:"swarm_manager"` // It is true on data-1 only.

	// Scheduler is data-1's fixed address, as host:port.
	Scheduler string `json:"scheduler"`
	// SchedulerCA is the PEM file of the CA that signed the scheduler's certificate.
	SchedulerCA string `json:"scheduler_ca"`
	// SchedulerCAKey is where the scheduler publishes that CA in the deploy bucket.
	SchedulerCAKey string `json:"scheduler_ca_key"`

	AutoScalingGroup     string   `json:"auto_scaling_group"`
	LaunchHook           string   `json:"launch_hook"`
	TerminateHook        string   `json:"terminate_hook"`
	TerminateHookTimeout duration `json:"terminate_hook_timeout"`

	DeployBucket  string `json:"deploy_bucket"`
	BackupsBucket string `json:"backups_bucket"` // Only data VMs have one.
	BackupHour    int    `json:"backup_hour"`    // The hour is in UTC.

	Network network        `json:"network"`
	Ingress *ingressConfig `json:"ingress"` // Only ingress VMs have it.

	CredentialsPort  int    `json:"credentials_port"`
	CredentialHelper string `json:"credential_helper"`
	SSMService       string `json:"ssm_service"`
	DockerSocket     string `json:"docker_socket"`
}

type network struct {
	VPC        string   `json:"vpc"`
	Resolver   string   `json:"resolver"`
	Managed    []string `json:"managed"`
	Containers []string `json:"containers"` // These are Docker's bridges.
}

type ingressConfig struct {
	Interface   string   `json:"interface"`
	ElasticIP   string   `json:"elastic_ip_allocation"`
	RouteTables []string `json:"route_tables"`
}

// duration reads "10m" and the like.
type duration struct{ time.Duration }

func (d *duration) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	d.Duration = v
	return nil
}

func loadConfig(path string) (config, error) {
	c := config{
		TerminateHookTimeout: duration{10 * time.Minute},
		BackupHour:           3,
		CredentialsPort:      51679,
		CredentialHelper:     "docker-credential-ecr-login",
		SSMService:           "amazon-ssm-agent",
		DockerSocket:         "/var/run/docker.sock",
	}
	f, err := os.Open(path)
	if err != nil {
		return config{}, err
	}
	defer f.Close()
	dec := json.NewDecoder(f)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return config{}, fmt.Errorf("%s: %w", path, err)
	}
	if len(c.Network.Containers) == 0 {
		// These are Docker's ranges from docs/network.md#dockers-ranges.
		c.Network.Containers = []string{"172.17.0.0/24", "172.18.0.0/24", "172.19.0.0/16"}
	}
	if err := c.validate(); err != nil {
		return config{}, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}

func (c config) firewall() firewall.Config {
	fw := firewall.Config{
		ContainerCIDRs: c.Network.Containers, VPCCIDR: c.Network.VPC, Resolver: c.Network.Resolver,
		ManagedCIDRs: c.Network.Managed, CredentialsPort: c.CredentialsPort,
	}
	if c.Ingress != nil {
		fw.NAT = &firewall.NAT{Interface: c.Ingress.Interface}
	}
	return fw
}

func (c config) validate() error {
	var errs []error
	fail := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }
	if c.App == "" {
		fail("no app")
	}
	switch c.Pool {
	case "ingress", "general", "compute", "data":
	default:
		fail("pool %q is not ingress, general, compute or data", c.Pool)
	}
	if c.Scheduler == "" {
		fail("no scheduler address")
	}
	if c.DeployBucket == "" {
		fail("no deploy bucket")
	}
	hooks := c.AutoScalingGroup != "" || c.LaunchHook != "" || c.TerminateHook != ""
	if c.Pool == "data" {
		if hooks {
			fail("a data VM is a plain instance, with no Auto Scaling group or hooks")
		}
		if c.BackupsBucket == "" {
			fail("a data VM needs a backups bucket")
		}
	} else {
		if c.AutoScalingGroup == "" || c.LaunchHook == "" || c.TerminateHook == "" {
			fail("a %s VM needs its Auto Scaling group and its launch and terminate hooks", c.Pool)
		}
		if c.SwarmManager {
			fail("only data-1 runs the swarm manager")
		}
	}
	if c.Pool == "ingress" {
		if c.Ingress == nil || c.Ingress.Interface == "" || c.Ingress.ElasticIP == "" || len(c.Ingress.RouteTables) == 0 {
			fail("an ingress VM needs its interface, its zone's Elastic IP and the zone's route tables")
		}
	} else if c.Ingress != nil {
		fail("only ingress VMs take over a zone")
	}
	if c.BackupHour < 0 || c.BackupHour > 23 {
		fail("backup hour %d", c.BackupHour)
	}
	if c.TerminateHookTimeout.Duration <= 0 {
		fail("terminate hook timeout must be positive")
	}
	if err := c.firewall().Validate(); err != nil {
		fail("network: %w", err)
	}
	return errors.Join(errs...)
}

// telemetryAddress is where Docker sends replicas' output: the telemetry collector on data-1,
// whose fixed address is the scheduler's (docs/telemetry.md).
func (c config) telemetryAddress() string {
	host, _, err := net.SplitHostPort(c.Scheduler)
	if err != nil {
		host = c.Scheduler
	}
	return net.JoinHostPort(host, strconv.Itoa(telemetry.ForwardPort))
}
