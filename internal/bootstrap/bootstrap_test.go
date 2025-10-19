package bootstrap

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunSuccess(t *testing.T) {
	ctx := context.Background()
	rootDir := t.TempDir()

	stackDir := filepath.Join(rootDir, "core-services", "bootstrap")
	if err := os.MkdirAll(stackDir, 0o755); err != nil {
		t.Fatalf("mkdir stack: %v", err)
	}

	mustWriteFile(t, filepath.Join(stackDir, "backend.tf"), "terraform {}")
	mustWriteFile(t, filepath.Join(rootDir, "globals.tfvars"), "global = \"value\"")
	mustWriteFile(t, filepath.Join(rootDir, "environment", "testenv.tfvars"), "env = \"value\"")
	mustWriteFile(t, filepath.Join(stackDir, "tfvars", "testenv.tfvars"), "stack = \"value\"")

	logPath := filepath.Join(rootDir, "terraform.log")
	opts := Options{
		RootDir:     rootDir,
		Environment: "testenv",
		AccountID:   "123456789012",
		Region:      "us-west-2",
	}
	expectedBucket := deriveBackendNames(opts)
	tfPath := newFakeTerraformBinary(t, rootDir, logPath, fmt.Sprintf(`{
  "state_bucket_id": {
    "value": "%s",
    "type": "string"
  }
}`, expectedBucket), false)
	requests := make(chan *http.Request, 5)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests <- r
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(server.Close)

	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")
	t.Setenv("AWS_REGION", "us-west-2")
	t.Setenv("AWS_ENDPOINT_URL_S3", server.URL)
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")

	opts.TerraformPath = tfPath

	if err := Run(ctx, opts); err != nil {
		t.Fatalf("Run returned error: %v", err)
	}

	expectFileExists(t, filepath.Join(stackDir, "backend.tf"))
	expectFileMissing(t, filepath.Join(stackDir, "backend.tf.disabled"))

	logContent := readFile(t, logPath)

	select {
	case req := <-requests:
		if req.Method != http.MethodHead {
			t.Fatalf("expected HEAD request, got %s", req.Method)
		}
		if req.URL.Path != fmt.Sprintf("/%s", expectedBucket) {
			t.Fatalf("expected bucket path /%s, got %s (log: %s)", expectedBucket, req.URL.Path, logContent)
		}
	default:
		t.Fatalf("expected HeadBucket call (log: %s)", logContent)
	}

	if !strings.Contains(logContent, "CMD:output -no-color -json") {
		t.Fatalf("expected output command to run, log: %s", logContent)
	}

	if !strings.Contains(logContent, "-backend=false") {
		t.Fatalf("expected init invocation with backend disabled, log: %s", logContent)
	}

	for _, vf := range []string{
		filepath.Join(rootDir, "globals.tfvars"),
		filepath.Join(rootDir, "environment", "testenv.tfvars"),
		filepath.Join(stackDir, "tfvars", "testenv.tfvars"),
	} {
		if !strings.Contains(logContent, fmt.Sprintf("-var-file=%s", vf)) {
			t.Fatalf("expected apply to include var file %s, log: %s", vf, logContent)
		}
	}

	if !strings.Contains(logContent, "CMD:apply -no-color -auto-approve") {
		t.Fatalf("expected apply to include auto-approve flag, log: %s", logContent)
	}

	if !strings.Contains(logContent, "-force-copy") {
		t.Fatalf("expected migration init with force-copy, log: %s", logContent)
	}

	if !strings.Contains(logContent, fmt.Sprintf("-backend-config=bucket=%s", expectedBucket)) {
		t.Fatalf("expected migration init to target bucket %s, log: %s", expectedBucket, logContent)
	}
}

func TestRunRestoresBackendOnApplyFailure(t *testing.T) {
	ctx := context.Background()
	rootDir := t.TempDir()

	stackDir := filepath.Join(rootDir, "core-services", "bootstrap")
	if err := os.MkdirAll(stackDir, 0o755); err != nil {
		t.Fatalf("mkdir stack: %v", err)
	}

	mustWriteFile(t, filepath.Join(stackDir, "backend.tf"), "terraform {}")

	logPath := filepath.Join(rootDir, "terraform.log")
	tfPath := newFakeTerraformBinary(t, rootDir, logPath, `{
  "state_bucket_id": {
    "value": "custom-bucket",
    "type": "string"
  }
}`, true)

	opts := Options{
		RootDir:       rootDir,
		TerraformPath: tfPath,
		Environment:   "dev",
		AccountID:     "123456789012",
		Region:        "us-west-2",
	}

	err := Run(ctx, opts)
	if err == nil {
		t.Fatal("expected error from Run when apply fails")
	}
	if !strings.Contains(err.Error(), "local apply failed") {
		t.Fatalf("expected apply failure error, got %v", err)
	}

	expectFileExists(t, filepath.Join(stackDir, "backend.tf"))
	expectFileMissing(t, filepath.Join(stackDir, "backend.tf.disabled"))
}

func TestRunFailsWhenBackendMissing(t *testing.T) {
	ctx := context.Background()
	rootDir := t.TempDir()

	stackDir := filepath.Join(rootDir, "core-services", "bootstrap")
	if err := os.MkdirAll(stackDir, 0o755); err != nil {
		t.Fatalf("mkdir stack: %v", err)
	}

	opts := Options{
		RootDir:       rootDir,
		TerraformPath: "/bin/true",
		Environment:   "dev",
		AccountID:     "123456789012",
		Region:        "us-west-2",
	}

	err := Run(ctx, opts)
	if err == nil {
		t.Fatal("expected error when backend.tf is missing")
	}
	if !strings.Contains(err.Error(), "backend.tf not found") {
		t.Fatalf("unexpected error: %v", err)
	}
	expectFileMissing(t, filepath.Join(stackDir, "backend.tf.disabled"))
}

func newFakeTerraformBinary(t *testing.T, dir, logPath, outputJSON string, failApply bool) string {
	t.Helper()

	path := filepath.Join(dir, "terraform-fake.sh")
	script := fmt.Sprintf(`#!/usr/bin/env bash
set -euo pipefail

LOG_FILE=%q
FAIL_APPLY=%t

printf "CMD:%%s\n" "$*" >> "$LOG_FILE"
printf "TF_CLI_ARGS_apply:%%s\n" "${TF_CLI_ARGS_apply-}" >> "$LOG_FILE"

case "$1" in
  init)
    exit 0
    ;;
  apply)
    if [[ "$FAIL_APPLY" == "true" ]]; then
      echo "forced apply failure" >&2
      exit 1
    fi
    exit 0
    ;;
  output)
    if [[ "${2:-}" == "-json" ]]; then
      cat <<'JSON'
%s
JSON
    fi
    exit 0
    ;;
  version)
    exit 0
    ;;
  *)
    exit 0
    ;;
esac
`, logPath, failApply, outputJSON)

	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write fake terraform: %v", err)
	}

	return path
}

func mustWriteFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir for %s: %v", path, err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write file %s: %v", path, err)
	}
}

func expectFileExists(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected %s to exist: %v", path, err)
	}
}

func expectFileMissing(t *testing.T, path string) {
	t.Helper()
	_, err := os.Stat(path)
	if err == nil || !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected %s to be missing, got err=%v", path, err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read file %s: %v", path, err)
	}
	return string(data)
}
