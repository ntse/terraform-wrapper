package versioning

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/hashicorp/go-version"
	"github.com/stretchr/testify/require"
)

func TestDetectConstraints(t *testing.T) {
	root := t.TempDir()

	stackA := filepath.Join(root, "stack-a")
	require.NoError(t, os.MkdirAll(stackA, 0o755))
	tfContent := `terraform {
  required_version = ">= 1.6.0, < 1.9.0"
}
`
	require.NoError(t, os.WriteFile(filepath.Join(stackA, "main.tf"), []byte(tfContent), 0o644))

	stackB := filepath.Join(root, "stack-b")
	require.NoError(t, os.MkdirAll(stackB, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(stackB, "variables.tf"), []byte("variable \"name\" {}"), 0o644))

	constraints, err := DetectConstraints(root, []string{stackA, stackB})
	require.NoError(t, err)

	require.Equal(t, ">= 1.6.0, < 1.9.0", constraints["stack-a"])
	require.Equal(t, defaultConstraint, constraints["stack-b"])
}

func TestParseTerraformVersion(t *testing.T) {
	tests := []struct {
		name    string
		output  string
		want    string
		wantErr bool
	}{
		{name: "standard", output: "Terraform v1.8.6\n", want: "1.8.6"},
		{name: "with platform", output: "Terraform v1.9.0 on darwin_amd64\n", want: "1.9.0"},
		{name: "invalid", output: "terraform whatever", wantErr: true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			v, err := parseTerraformVersion([]byte(tc.output))
			if tc.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.want, v.String())
		})
	}
}

func TestLockFileRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".terraform-version.lock.json")

	lock := LockFile{
		Version:          "1.8.6",
		UsedSystemBinary: true,
		BinaryPath:       "/usr/local/bin/terraform",
		DetectedFrom:     []string{"./b", "a"},
	}

	require.NoError(t, WriteLockFile(path, lock))

	read, err := ReadLockFile(path)
	require.NoError(t, err)
	require.Equal(t, "1.8.6", read.Version)
	require.True(t, read.UsedSystemBinary)
	require.Equal(t, []string{"a", "b"}, read.DetectedFrom)
}

func TestResolveInstallVersionPrefersPreferred(t *testing.T) {
	preferred, err := version.NewVersion("1.7.5")
	require.NoError(t, err)

	got, err := resolveInstallVersion(context.Background(), []string{">= 1.6.0"}, preferred)
	require.NoError(t, err)
	require.Equal(t, preferred.String(), got.String())
}

func TestResolveInstallVersionSelectsLatest(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "application/json")
		payload := map[string]any{
			"versions": map[string]any{
				"1.5.0":       map[string]string{"version": "1.5.0"},
				"1.6.0":       map[string]string{"version": "1.6.0"},
				"1.6.0-beta1": map[string]string{"version": "1.6.0-beta1"},
			},
		}
		require.NoError(t, json.NewEncoder(w).Encode(payload))
	}))
	t.Cleanup(server.Close)

	prevClient := httpClient
	httpClient = &http.Client{
		Timeout: 5 * time.Second,
		Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			req.URL.Scheme = "http"
			req.URL.Host = server.Listener.Addr().String()
			return http.DefaultTransport.RoundTrip(req)
		}),
	}
	t.Cleanup(func() { httpClient = prevClient })

	got, err := resolveInstallVersion(context.Background(), []string{">= 1.5.0"}, nil)
	require.NoError(t, err)
	require.Equal(t, "1.6.0", got.String())
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}
