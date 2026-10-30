package docker

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
)

// registryOf is the registry host an image is pulled from.
func registryOf(image string) string {
	first, _, found := strings.Cut(image, "/")
	if !found {
		return "docker.io"
	}
	if strings.ContainsAny(first, ".:") || first == "localhost" {
		return first
	}
	return "docker.io"
}

// CredentialHelper gets ECR credentials from Amazon's Docker credential helper. The helper
// is a docker CLI feature, not the daemon's, so the agent runs it itself.
type CredentialHelper struct {
	// Path is the helper's command, such as docker-credential-ecr-login.
	Path string
}

// Check, a boot check, fails when the helper isn't installed.
func (h CredentialHelper) Check() error {
	if _, err := exec.LookPath(h.Path); err != nil {
		return fmt.Errorf("credential helper: %w", err)
	}
	return nil
}

func isECR(registry string) bool {
	return strings.Contains(registry, ".dkr.ecr.") && strings.Contains(registry, ".amazonaws.com")
}

// Credentials asks the helper for an ECR registry's credentials. Other registries are
// pulled from without any.
func (h CredentialHelper) Credentials(ctx context.Context, registry string) (string, string, bool, error) {
	if !isECR(registry) {
		return "", "", false, nil
	}
	cmd := exec.CommandContext(ctx, h.Path, "get")
	cmd.Stdin = strings.NewReader(registry)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return "", "", false, fmt.Errorf("%s get %s: %w: %s", h.Path, registry, err, strings.TrimSpace(stderr.String()))
	}
	var out struct {
		Username string
		Secret   string
	}
	if err := json.Unmarshal(stdout.Bytes(), &out); err != nil {
		return "", "", false, fmt.Errorf("%s get %s: %w", h.Path, registry, err)
	}
	return out.Username, out.Secret, true, nil
}
