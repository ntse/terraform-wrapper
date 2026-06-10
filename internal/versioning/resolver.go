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
	SystemBinaryPath string
	Constraints      map[string]string
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
	if _, err := fmt.Fprintln(stdout, "Detected Terraform version requirements:"); err != nil {
		return nil, fmt.Errorf("write constraint header: %w", err)
	}
	for _, stack := range stackNames {
		if _, err := fmt.Fprintf(stdout, "- %s: %s\n", stack, constraintsByStack[stack]); err != nil {
			return nil, fmt.Errorf("write constraint for %s: %w", stack, err)
		}
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
		if _, logErr := fmt.Fprintf(stderr, "warning: failed to detect system Terraform version: %v\n", systemErr); logErr != nil {
			return nil, fmt.Errorf("write system detection warning: %w", logErr)
		}
		systemErr = ErrTerraformNotFound
	}

	if opts.UseSystemOnly {
		if systemErr != nil {
			return nil, fmt.Errorf("system terraform binary required but not found: %w", systemErr)
		}
		if opts.PinnedVersion != nil && !systemVersion.Equal(opts.PinnedVersion) {
			if _, logErr := fmt.Fprintf(stderr, "warning: system terraform version %s differs from pinned %s\n", systemVersion, opts.PinnedVersion); logErr != nil {
				return nil, fmt.Errorf("write system mismatch warning: %w", logErr)
			}
		}
		if ok, err := IsVersionCompatible(systemVersion, constraintStrings); err != nil {
			return nil, err
		} else if !ok {
			if _, logErr := fmt.Fprintf(stderr, "warning: system terraform %s does not satisfy all constraints\n", systemVersion); logErr != nil {
				return nil, fmt.Errorf("write system constraint warning: %w", logErr)
			}
		} else {
			if _, logErr := fmt.Fprintf(stdout, "System Terraform v%s detected — satisfies all constraints.\n", systemVersion); logErr != nil {
				return nil, fmt.Errorf("write system success message: %w", logErr)
			}
		}
		return finalizeResolution(stdout, constraintsByStack, systemVersion, systemPath, true)
	}

	if opts.ForceInstall {
		versionToInstall, err := resolveInstallVersion(ctx, constraintStrings, opts.PinnedVersion)
		if err != nil {
			return nil, err
		}
		if _, logErr := fmt.Fprintf(stdout, "Installing Terraform v%s (forced install).\n", versionToInstall); logErr != nil {
			return nil, fmt.Errorf("write forced install message: %w", logErr)
		}
		path, err := ensureVersionInstalled(ctx, versionToInstall)
		if err != nil {
			return nil, err
		}
		return finalizeResolution(stdout, constraintsByStack, versionToInstall, path, false)
	}

	if systemErr == nil {
		ok, err := IsVersionCompatible(systemVersion, constraintStrings)
		if err != nil {
			return nil, err
		}
		if ok {
			if _, logErr := fmt.Fprintf(stdout, "System Terraform v%s detected — satisfies all constraints.\n", systemVersion); logErr != nil {
				return nil, fmt.Errorf("write system compatibility message: %w", logErr)
			}
			return finalizeResolution(stdout, constraintsByStack, systemVersion, systemPath, true)
		}
		if _, logErr := fmt.Fprintf(stdout, "System Terraform v%s does not satisfy all constraints.\n", systemVersion); logErr != nil {
			return nil, fmt.Errorf("write system incompatibility message: %w", logErr)
		}
		if opts.DisableInstall {
			return nil, fmt.Errorf("system terraform %s incompatible and installation is disabled", systemVersion)
		}
	} else if errors.Is(systemErr, ErrTerraformNotFound) {
		if _, logErr := fmt.Fprintln(stdout, "System Terraform binary not found."); logErr != nil {
			return nil, fmt.Errorf("write system not found message: %w", logErr)
		}
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
		if _, logErr := fmt.Fprintf(stdout, "Installing Terraform v%s (latest compatible).\n", versionToInstall); logErr != nil {
			return nil, fmt.Errorf("write latest install message: %w", logErr)
		}
	} else {
		if _, logErr := fmt.Fprintf(stdout, "Installing Terraform v%s...\n", versionToInstall); logErr != nil {
			return nil, fmt.Errorf("write install message: %w", logErr)
		}
	}
	path, err := ensureVersionInstalled(ctx, versionToInstall)
	if err != nil {
		return nil, err
	}
	return finalizeResolution(stdout, constraintsByStack, versionToInstall, path, false)
}

func finalizeResolution(stdout io.Writer, constraints map[string]string, version *version.Version, binaryPath string, usedSystem bool) (*ResolveResult, error) {
	if binaryPath == "" {
		return nil, errors.New("binary path cannot be empty")
	}
	if stdout == nil {
		stdout = os.Stdout
	}

	if usedSystem {
		if _, logErr := fmt.Fprintf(stdout, "Using system binary: %s\n", binaryPath); logErr != nil {
			return nil, fmt.Errorf("write system binary info: %w", logErr)
		}
	} else {
		if _, logErr := fmt.Fprintf(stdout, "Using installed binary: %s\n", binaryPath); logErr != nil {
			return nil, fmt.Errorf("write installed binary info: %w", logErr)
		}
	}
	if _, logErr := fmt.Fprintf(stdout, "Resolved version: %s\n", version.String()); logErr != nil {
		return nil, fmt.Errorf("write resolved version info: %w", logErr)
	}

	return &ResolveResult{
		BinaryPath:       binaryPath,
		Version:          version,
		UsedSystemBinary: usedSystem,
		SystemBinaryPath: binaryPath,
		Constraints:      constraints,
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
