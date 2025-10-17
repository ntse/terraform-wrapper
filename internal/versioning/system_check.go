package versioning

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"sort"
	"strings"

	"github.com/hashicorp/go-version"
)

var (
	// ErrTerraformNotFound indicates that terraform binary could not be located in PATH.
	ErrTerraformNotFound = errors.New("terraform binary not found in PATH")

	tfVersionPattern = regexp.MustCompile(`^Terraform\s+v?([0-9A-Za-z\.\-\+]+)`)
)

// DetectSystemTerraformVersion resolves the terraform binary from PATH, executes `terraform -version`,
// and returns the parsed semantic version along with the binary path.
func DetectSystemTerraformVersion(ctx context.Context) (*version.Version, string, error) {
	binaryPath, err := exec.LookPath("terraform")
	if err != nil {
		var execErr *exec.Error
		if errors.As(err, &execErr) && execErr.Err == exec.ErrNotFound {
			return nil, "", ErrTerraformNotFound
		}
		return nil, "", fmt.Errorf("locate terraform binary: %w", err)
	}

	cmd := exec.CommandContext(ctx, binaryPath, "-version")
	output, err := cmd.CombinedOutput()
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return nil, "", err
		}
		return nil, "", fmt.Errorf("terraform -version failed: %w (output: %s)", err, bytes.TrimSpace(output))
	}

	v, err := parseTerraformVersion(output)
	if err != nil {
		return nil, "", err
	}

	return v, binaryPath, nil
}

func parseTerraformVersion(output []byte) (*version.Version, error) {
	scanner := bufio.NewScanner(bytes.NewReader(output))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		m := tfVersionPattern.FindStringSubmatch(line)
		if len(m) != 2 {
			continue
		}
		return version.NewVersion(strings.TrimPrefix(m[1], "v"))
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scan terraform version output: %w", err)
	}
	return nil, fmt.Errorf("failed to detect terraform version from output: %q", string(output))
}

// IsVersionCompatible verifies whether the provided version satisfies all specified constraints.
func IsVersionCompatible(systemVersion *version.Version, constraints []string) (bool, error) {
	if systemVersion == nil {
		return false, errors.New("system version is nil")
	}
	all, err := mergeConstraints(constraints)
	if err != nil {
		return false, err
	}
	for _, constraint := range all {
		if !constraint.Check(systemVersion) {
			return false, nil
		}
	}
	return true, nil
}

func mergeConstraints(values []string) (version.Constraints, error) {
	if len(values) == 0 {
		return version.Constraints{}, nil
	}
	seen := make(map[string]struct{}, len(values))
	var merged version.Constraints
	sort.Strings(values)
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		c, err := version.NewConstraint(value)
		if err != nil {
			return nil, fmt.Errorf("parse constraint %q: %w", value, err)
		}
		merged = append(merged, c...)
	}
	return merged, nil
}
