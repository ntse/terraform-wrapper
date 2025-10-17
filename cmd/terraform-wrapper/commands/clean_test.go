package commands

import (
	"os"
	"path/filepath"
	"testing"

	"terraform-wrapper/internal/graph"
)

func TestCleanStackArtifacts(t *testing.T) {
	root := t.TempDir()
	stack := filepath.Join(root, "stack")
	terraformDir := filepath.Join(stack, ".terraform")
	lockPath := filepath.Join(stack, "terraform.lock.hcl")

	if err := os.MkdirAll(filepath.Join(terraformDir, "plugins"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.MkdirAll(stack, 0o755); err != nil {
		t.Fatalf("mkdir stack: %v", err)
	}
	if err := os.WriteFile(lockPath, []byte("lock"), 0o644); err != nil {
		t.Fatalf("write lock: %v", err)
	}

	if err := cleanStackArtifacts(stack); err != nil {
		t.Fatalf("clean stack: %v", err)
	}

	if _, err := os.Stat(terraformDir); !os.IsNotExist(err) {
		t.Fatalf(".terraform directory still exists")
	}
	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Fatalf("terraform.lock.hcl still exists")
	}
}

func TestCleanStacksMultiple(t *testing.T) {
	root := t.TempDir()
	var stacks []*graph.Stack

	for i := 0; i < 2; i++ {
		stackDir := filepath.Join(root, "stack", string(rune('a'+i)))
		if err := os.MkdirAll(filepath.Join(stackDir, ".terraform"), 0o755); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		stacks = append(stacks, &graph.Stack{Path: stackDir})
	}

	if err := cleanStacks(stacks); err != nil {
		t.Fatalf("cleanStacks: %v", err)
	}

	for _, stack := range stacks {
		if _, err := os.Stat(filepath.Join(stack.Path, ".terraform")); !os.IsNotExist(err) {
			t.Fatalf(".terraform still exists for %s", stack.Path)
		}
	}
}
