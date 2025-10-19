package versioning

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/hashicorp/go-version"
	"github.com/hashicorp/hc-install/product"
	"github.com/hashicorp/hc-install/releases"
)

const (
	terraformReleasesIndex = "https://releases.hashicorp.com/terraform/index.json"
)

var httpClient = &http.Client{Timeout: 15 * time.Second}

type releasesIndex struct {
	Versions map[string]struct {
		Version string `json:"version"`
	} `json:"versions"`
}

func resolveInstallVersion(ctx context.Context, constraintStrings []string, preferred *version.Version) (*version.Version, error) {
	constraints, err := mergeConstraints(constraintStrings)
	if err != nil {
		return nil, err
	}

	if preferred != nil {
		if constraints.Check(preferred) {
			return preferred, nil
		}
	}

	available, err := fetchAvailableVersions(ctx)
	if err != nil {
		return nil, err
	}

	sort.Sort(available)
	for i := len(available) - 1; i >= 0; i-- {
		v := available[i]
		if v.Prerelease() != "" || v.Metadata() != "" {
			continue
		}
		if constraints.Check(v) {
			return v, nil
		}
	}

	return nil, fmt.Errorf("no Terraform versions satisfy constraints %v", constraintStrings)
}

func fetchAvailableVersions(ctx context.Context) (versions version.Collection, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, terraformReleasesIndex, nil)
	if err != nil {
		return nil, fmt.Errorf("build releases request: %w", err)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch Terraform releases: %w", err)
	}
	defer func() {
		if cerr := resp.Body.Close(); cerr != nil && err == nil {
			err = fmt.Errorf("close Terraform releases body: %w", cerr)
		}
	}()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return nil, fmt.Errorf("fetch Terraform releases: unexpected status %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}

	var payload releasesIndex
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("parse Terraform releases index: %w", err)
	}

	if len(payload.Versions) == 0 {
		return nil, errors.New("Terraform releases index empty") //nolint:staticcheck // "Terraform" is a proper noun
	}

	for key, entry := range payload.Versions {
		verStr := strings.TrimSpace(entry.Version)
		if verStr == "" {
			verStr = strings.TrimSpace(key)
		}
		if verStr == "" {
			continue
		}
		v, err := version.NewVersion(verStr)
		if err != nil {
			continue
		}
		versions = append(versions, v)
	}

	if len(versions) == 0 {
		return nil, errors.New("no parseable Terraform versions in index")
	}

	return versions, nil
}

func ensureVersionInstalled(ctx context.Context, v *version.Version) (string, error) {
	if v == nil {
		return "", errors.New("version to install is nil")
	}
	cacheDir, err := cacheDirectory()
	if err != nil {
		return "", err
	}

	installDir := filepath.Join(cacheDir, v.String())
	binaryPath := filepath.Join(installDir, product.Terraform.BinaryName())

	if info, err := os.Stat(binaryPath); err == nil && !info.IsDir() {
		return binaryPath, nil
	}

	if err := os.MkdirAll(installDir, 0o755); err != nil {
		return "", fmt.Errorf("create install directory %s: %w", installDir, err)
	}

	installer := &releases.ExactVersion{
		Product:    product.Terraform,
		Version:    v,
		InstallDir: installDir,
	}

	path, err := installer.Install(ctx)
	if err != nil {
		return "", fmt.Errorf("install terraform %s: %w", v.String(), err)
	}

	return path, nil
}

func cacheDirectory() (string, error) {
	path, err := cacheRoot()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(path, 0o755); err != nil {
		return "", fmt.Errorf("create cache directory %s: %w", path, err)
	}
	return path, nil
}

func cacheRoot() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("determine user home: %w", err)
	}
	return filepath.Join(home, ".terraform-wrapper", "versions"), nil
}

func cachedBinaryPath(v *version.Version) (string, error) {
	if v == nil {
		return "", errors.New("version is nil")
	}
	root, err := cacheRoot()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, v.String(), product.Terraform.BinaryName()), nil
}
