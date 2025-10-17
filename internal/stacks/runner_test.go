package stacks

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestVarFilesAndBackendConfig(t *testing.T) {
	root := t.TempDir()
	stackDir := filepath.Join(root, "network")
	require.NoError(t, os.MkdirAll(filepath.Join(stackDir, "tfvars"), 0o755))
	require.NoError(t, os.MkdirAll(filepath.Join(root, "environment"), 0o755))
	require.NoError(t, os.MkdirAll(stackDir, 0o755))

	require.NoError(t, os.WriteFile(filepath.Join(root, "globals.tfvars"), []byte("global = true"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(root, "environment", "dev.tfvars"), []byte("env = true"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(stackDir, "tfvars", "dev.tfvars"), []byte("stack = true"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(stackDir, "main.tf"), []byte("terraform {}"), 0o644))

	r := &Runner{root: root, environment: "dev", accountID: "123", region: "eu-west-2"}

	files := r.VarFilesFor(stackDir)
	require.Contains(t, files, filepath.Join(root, "globals.tfvars"))
	require.Contains(t, files, filepath.Join(root, "environment", "dev.tfvars"))
	require.Contains(t, files, filepath.Join(stackDir, "tfvars", "dev.tfvars"))

	backend := r.BackendConfig(stackDir)
	require.Equal(t, map[string]string{
		"bucket":  "123-eu-west-2-state",
		"key":     "dev/network/terraform.tfstate",
		"region":  "eu-west-2",
		"encrypt": "true",
	}, backend)
}

func TestNewRunnerValidatesInputs(t *testing.T) {
	ctx := context.Background()
	_, err := NewRunner(ctx, RunnerOptions{RootDir: t.TempDir(), AccountID: "", Region: "eu"})
	require.Error(t, err)
}

func TestNewRunnerUsesInjectedTerraformPath(t *testing.T) {
	ctx := context.Background()
	opts := RunnerOptions{
		RootDir:       t.TempDir(),
		Environment:   "dev",
		AccountID:     "123",
		Region:        "eu",
		TerraformPath: "/custom/terraform",
	}

	r, err := NewRunner(ctx, opts)
	require.NoError(t, err)
	require.Equal(t, "/custom/terraform", r.terraformPath)
}
