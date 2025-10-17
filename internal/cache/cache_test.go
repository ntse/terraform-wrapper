package cache_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"terraform-wrapper/internal/cache"
)

func TestPlanDirAndFiles(t *testing.T) {
	t.Parallel()

	root := "/workspace"
	dir := cache.PlanDir(root, "dev", "core-services/network")
	require.Equal(t, filepath.Join(root, ".terraform-wrapper", "cache", "dev", "core-services/network"), dir)

	plan, hash := cache.PlanFiles(root, "dev", "core-services/network")
	require.Equal(t, filepath.Join(dir, "plan.tfplan"), plan)
	require.Equal(t, filepath.Join(dir, "plan.hash"), hash)
}

func TestSaveAndLoadHash(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()
	hashPath := filepath.Join(tmp, "plan.hash")

	original := []byte{0xde, 0xad, 0xbe, 0xef}
	require.NoError(t, cache.SaveHash(hashPath, original))

	loaded, err := cache.LoadHash(hashPath)
	require.NoError(t, err)
	require.Equal(t, original, loaded)
}

func TestComputeHashDetectsChanges(t *testing.T) {
	t.Parallel()

	tmp := t.TempDir()

	fileA := filepath.Join(tmp, "a.tf")
	fileB := filepath.Join(tmp, "b.tfvars")

	writeFile(t, fileA, "resource \"null_resource\" \"a\" {}")
	writeFile(t, fileB, "foo = \"bar\"")

	firstHash, err := cache.ComputeHash([]string{fileA, fileB})
	require.NoError(t, err)
	require.Len(t, firstHash, 32)

	// Reordering should not affect hash.
	secondHash, err := cache.ComputeHash([]string{fileB, fileA})
	require.NoError(t, err)
	require.Equal(t, firstHash, secondHash)

	// Modifying a file should change hash.
	writeFile(t, fileB, "foo = \"baz\"")
	thirdHash, err := cache.ComputeHash([]string{fileA, fileB})
	require.NoError(t, err)
	require.NotEqual(t, firstHash, thirdHash)
}

func TestStackContentFiles(t *testing.T) {
	t.Parallel()

	stackDir := t.TempDir()

	files := map[string]string{
		"main.tf":       "terraform {}",
		"variables.tf":  "variable \"x\" {}",
		"locals.tfvars": "x = 1",
		"README.md":     "# ignored",
	}

	for name, content := range files {
		writeFile(t, filepath.Join(stackDir, name), content)
	}

	// Create .terraform dir that should be skipped.
	require.NoError(t, os.Mkdir(filepath.Join(stackDir, ".terraform"), 0o755))
	writeFile(t, filepath.Join(stackDir, ".terraform", "state.tf"), "ignored")

	extraDir := t.TempDir()
	extras := []string{filepath.Join(extraDir, "custom.tfvars")}
	writeFile(t, extras[0], "custom = true")

	collected, err := cache.StackContentFiles(stackDir, extras)
	require.NoError(t, err)

	require.Len(t, collected, 4) // three *.tf / *.tfvars + extras[0]
	require.Contains(t, collected, filepath.Join(stackDir, "main.tf"))
	require.Contains(t, collected, filepath.Join(stackDir, "variables.tf"))
	require.Contains(t, collected, filepath.Join(stackDir, "locals.tfvars"))
	require.Contains(t, collected, extras[0])

	// ensure extras appended at the end
	require.Equal(t, extras[0], collected[len(collected)-1])
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(strings.TrimSpace(body)), 0o644))
}
