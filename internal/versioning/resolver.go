package versioning

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/hashicorp/go-version"
)

type ResolveOptions struct {
	RootDir        string
	StackPaths     []string
	Stdout         io.Writer
	Stderr         io.Writer
	ForceInstall   bool
	UseSystemOnly  bool
	DisableInstall bool
	PinnedVersion  *version.Version
}

type ResolveResult struct {
	BinaryPath       string
	Version          *version.Version
	UsedSystemBinary bool
}

func ResolveTerraformBinary(ctx context.Context, opts ResolveOptions) (*ResolveResult, error) {
	if len(opts.StackPaths) == 0 {
		return nil, errors.New("no stack paths supplied")
	}
	if opts.ForceInstall && opts.UseSystemOnly {
		return nil, errors.New("TFWRAPPER_FORCE_INSTALL and TFWRAPPER_USE_SYSTEM_TERRAFORM cannot both be set")
	}
	if opts.ForceInstall && opts.DisableInstall {
		return nil, errors.New("TFWRAPPER_FORCE_INSTALL conflicts with TFWRAPPER_DISABLE_INSTALL")
	}

	stdout := opts.Stdout
	if stdout == nil {
		stdout = os.Stdout
	}
	stderr := opts.Stderr
	if stderr == nil {
		stderr = os.Stderr
	}

	constraintsByStack, err := DetectConstraints(opts.RootDir, opts.StackPaths)
	if err != nil {
		return nil, err
	}

	stackNames := sortedKeys(constraintsByStack)
	_, _ = fmt.Fprintln(stdout, "Detected Terraform version requirements:")
	for _, stack := range stackNames {
		_, _ = fmt.Fprintf(stdout, "- %s: %s\n", stack, constraintsByStack[stack])
	}

	constraintStrings := make([]string, 0, len(stackNames))
	for _, stack := range stackNames {
		constraintStrings = append(constraintStrings, constraintsByStack[stack])
	}

	if opts.PinnedVersion != nil {
		if ok, cerr := IsVersionCompatible(opts.PinnedVersion, constraintStrings); cerr != nil {
			return nil, cerr
		} else if !ok {
			return nil, fmt.Errorf("pinned Terraform version %s does not satisfy stack constraints", opts.PinnedVersion)
		}
	}

	systemVersion, systemPath, systemErr := DetectSystemTerraformVersion(ctx)
	if systemErr != nil && !errors.Is(systemErr, ErrTerraformNotFound) {
		_, _ = fmt.Fprintf(stderr, "warning: failed to detect system Terraform version: %v\n", systemErr)
		systemErr = ErrTerraformNotFound
	}

	if opts.UseSystemOnly {
		if systemErr != nil {
			return nil, fmt.Errorf("system terraform binary required but not found: %w", systemErr)
		}
		if opts.PinnedVersion != nil && !systemVersion.Equal(opts.PinnedVersion) {
			_, _ = fmt.Fprintf(stderr, "warning: system terraform version %s differs from pinned %s\n", systemVersion, opts.PinnedVersion)
		}
		if ok, err := IsVersionCompatible(systemVersion, constraintStrings); err != nil {
			return nil, err
		} else if !ok {
			_, _ = fmt.Fprintf(stderr, "warning: system terraform %s does not satisfy all constraints\n", systemVersion)
		} else {
			_, _ = fmt.Fprintf(stdout, "System Terraform v%s detected — satisfies all constraints.\n", systemVersion)
		}
		return finalizeResolution(stdout, systemVersion, systemPath, true)
	}

	if opts.ForceInstall {
		versionToInstall, err := resolveInstallVersion(ctx, constraintStrings, opts.PinnedVersion)
		if err != nil {
			return nil, err
		}
		_, _ = fmt.Fprintf(stdout, "Installing Terraform v%s (forced install).\n", versionToInstall)
		path, err := ensureVersionInstalled(ctx, versionToInstall)
		if err != nil {
			return nil, err
		}
		return finalizeResolution(stdout, versionToInstall, path, false)
	}

	if systemErr == nil {
		ok, err := IsVersionCompatible(systemVersion, constraintStrings)
		if err != nil {
			return nil, err
		}
		if ok {
			_, _ = fmt.Fprintf(stdout, "System Terraform v%s detected — satisfies all constraints.\n", systemVersion)
			return finalizeResolution(stdout, systemVersion, systemPath, true)
		}
		_, _ = fmt.Fprintf(stdout, "System Terraform v%s does not satisfy all constraints.\n", systemVersion)
		if opts.DisableInstall {
			return nil, fmt.Errorf("system terraform %s incompatible and installation is disabled", systemVersion)
		}
	} else if errors.Is(systemErr, ErrTerraformNotFound) {
		_, _ = fmt.Fprintln(stdout, "System Terraform binary not found.")
		if opts.DisableInstall {
			return nil, fmt.Errorf("terraform binary not found and installation disabled")
		}
	} else {
		if opts.DisableInstall {
			return nil, fmt.Errorf("failed to detect Terraform version and installation disabled: %w", systemErr)
		}
	}

	versionToInstall, err := resolveInstallVersion(ctx, constraintStrings, opts.PinnedVersion)
	if err != nil {
		return nil, err
	}
	if systemErr == nil {
		_, _ = fmt.Fprintf(stdout, "Installing Terraform v%s (latest compatible).\n", versionToInstall)
	} else {
		_, _ = fmt.Fprintf(stdout, "Installing Terraform v%s...\n", versionToInstall)
	}
	path, err := ensureVersionInstalled(ctx, versionToInstall)
	if err != nil {
		return nil, err
	}
	return finalizeResolution(stdout, versionToInstall, path, false)
}

func finalizeResolution(stdout io.Writer, version *version.Version, binaryPath string, usedSystem bool) (*ResolveResult, error) {
	if binaryPath == "" {
		return nil, errors.New("binary path cannot be empty")
	}
	if stdout == nil {
		stdout = os.Stdout
	}

	if usedSystem {
		_, _ = fmt.Fprintf(stdout, "Using system binary: %s\n", binaryPath)
	} else {
		_, _ = fmt.Fprintf(stdout, "Using installed binary: %s\n", binaryPath)
	}
	_, _ = fmt.Fprintf(stdout, "Resolved version: %s\n", version.String())

	return &ResolveResult{
		BinaryPath:       binaryPath,
		Version:          version,
		UsedSystemBinary: usedSystem,
	}, nil
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for i, key := range keys {
		if key == "." || key == "" {
			keys[i] = "."
			continue
		}
		keys[i] = strings.TrimPrefix(strings.ReplaceAll(key, "\\", "/"), "./")
	}
	return keys
}
