package executor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"terraform-wrapper/internal/graph"
	"terraform-wrapper/internal/stacks"
)

func TestRunAllApplyRespectsDependencies(t *testing.T) {
	root := t.TempDir()
	factory := newFakeRunnerFactory(root)
	withFakeRunner(t, factory)

	stackA := filepath.Join(root, "a")
	stackB := filepath.Join(root, "b")
	stackC := filepath.Join(root, "c")

	g := graph.Graph{
		stackA: {Path: stackA},
		stackB: {Path: stackB, Dependencies: []string{stackA}},
		stackC: {Path: stackC, Dependencies: []string{stackB}},
	}

	opts := Options{
		RootDir:       root,
		Environment:   "dev",
		AccountID:     "123456789012",
		Region:        "eu-west-2",
		Parallelism:   2,
		TerraformPath: "/tmp/terraform",
	}

	summary, err := RunAll(context.Background(), g, opts, OperationApply)
	require.NoError(t, err)
	require.Equal(t, 3, summary.Executed)
	require.Nil(t, summary.Failed)

	records := factory.records()
	require.Len(t, records, 3)

	index := indexOf(records)
	require.Less(t, index["apply:a"], index["apply:b"])
	require.Less(t, index["apply:b"], index["apply:c"])
}

func TestRunAllStopsOnError(t *testing.T) {
	root := t.TempDir()
	factory := newFakeRunnerFactory(root)
	factory.failures["b"] = errors.New("boom")
	withFakeRunner(t, factory)

	stackA := filepath.Join(root, "a")
	stackB := filepath.Join(root, "b")

	g := graph.Graph{
		stackA: {Path: stackA},
		stackB: {Path: stackB, Dependencies: []string{stackA}},
	}

	opts := Options{
		RootDir:       root,
		Environment:   "dev",
		AccountID:     "123",
		Region:        "eu-west-2",
		TerraformPath: "/tmp/terraform",
	}

	summary, err := RunAll(context.Background(), g, opts, OperationApply)
	require.Error(t, err)
	require.NotNil(t, summary.Failed)
	require.Contains(t, summary.Failed, "b")
}

func TestPlanStackInvokesRunner(t *testing.T) {
	root := t.TempDir()
	factory := newFakeRunnerFactory(root)
	withFakeRunner(t, factory)

	stackDir := filepath.Join(root, "stack")
	require.NoError(t, os.MkdirAll(stackDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(stackDir, "main.tf"), []byte("terraform {}"), 0o644))

	stack := &graph.Stack{Path: stackDir}
	opts := Options{
		RootDir:       root,
		Environment:   "dev",
		AccountID:     "123",
		Region:        "eu-west-2",
		TerraformPath: "/tmp/terraform",
	}

	summary, err := PlanStack(context.Background(), stack, opts)
	require.NoError(t, err)
	require.Equal(t, 1, summary.Executed)
	require.Contains(t, factory.records(), "plan:stack")
}

func TestRefreshStackInvokesRunner(t *testing.T) {
	root := t.TempDir()
	factory := newFakeRunnerFactory(root)
	withFakeRunner(t, factory)

	stackDir := filepath.Join(root, "stack")
	require.NoError(t, os.MkdirAll(stackDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(stackDir, "main.tf"), []byte("terraform {}"), 0o644))

	stack := &graph.Stack{Path: stackDir}
	opts := Options{
		RootDir:       root,
		Environment:   "dev",
		AccountID:     "123",
		Region:        "eu-west-2",
		TerraformPath: "/tmp/terraform",
	}

	summary, err := RefreshStack(context.Background(), stack, opts)
	require.NoError(t, err)
	require.Equal(t, 1, summary.Executed)
	require.Contains(t, factory.records(), "refresh:stack")
}

func TestRefreshAllRespectsDependencies(t *testing.T) {
	root := t.TempDir()
	factory := newFakeRunnerFactory(root)
	withFakeRunner(t, factory)

	stackA := filepath.Join(root, "a")
	stackB := filepath.Join(root, "b")

	g := graph.Graph{
		stackA: {Path: stackA},
		stackB: {Path: stackB, Dependencies: []string{stackA}},
	}

	opts := Options{
		RootDir:       root,
		Environment:   "dev",
		AccountID:     "123456789012",
		Region:        "eu-west-2",
		Parallelism:   2,
		TerraformPath: "/tmp/terraform",
	}

	summary, err := RunAll(context.Background(), g, opts, OperationRefresh)
	require.NoError(t, err)
	require.Equal(t, 2, summary.Executed)
	require.Nil(t, summary.Failed)

	index := indexOf(factory.records())
	require.Less(t, index["refresh:a"], index["refresh:b"])
}

// --- test helpers ---

type fakeRunnerFactory struct {
	mu        sync.Mutex
	recording []string
	failures  map[string]error
	root      string
}

func newFakeRunnerFactory(root string) *fakeRunnerFactory {
	return &fakeRunnerFactory{
		failures: make(map[string]error),
		root:     root,
	}
}

func (f *fakeRunnerFactory) new(ctx context.Context, opts stacks.RunnerOptions) (runner, error) {
	return &fakeRunner{factory: f, root: opts.RootDir}, nil
}

func (f *fakeRunnerFactory) record(op, stack string, err error) error {
	rel, _ := filepath.Rel(f.root, stack)
	rel = filepath.ToSlash(rel)
	entry := fmt.Sprintf("%s:%s", op, rel)

	f.mu.Lock()
	defer f.mu.Unlock()
	f.recording = append(f.recording, entry)
	if e, ok := f.failures[rel]; ok {
		return e
	}
	return err
}

func (f *fakeRunnerFactory) records() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]string, len(f.recording))
	copy(out, f.recording)
	return out
}

type fakeRunner struct {
	factory *fakeRunnerFactory
	root    string
}

func (r *fakeRunner) Apply(ctx context.Context, stack string) error {
	return r.factory.record("apply", stack, nil)
}

func (r *fakeRunner) Destroy(ctx context.Context, stack string) error {
	return r.factory.record("destroy", stack, nil)
}

func (r *fakeRunner) InitOnly(ctx context.Context, stack string, upgrade bool) error {
	return r.factory.record("init", stack, nil)
}

func (r *fakeRunner) Plan(ctx context.Context, stack string) error {
	return r.factory.record("plan", stack, nil)
}

func (r *fakeRunner) Refresh(ctx context.Context, stack string) error {
	return r.factory.record("refresh", stack, nil)
}

func withFakeRunner(t *testing.T, factory *fakeRunnerFactory) {
	origRunner := newRunner

	newRunner = factory.new

	t.Cleanup(func() {
		newRunner = origRunner
	})
}

func indexOf(records []string) map[string]int {
	result := make(map[string]int, len(records))
	for i, r := range records {
		result[r] = i
	}
	return result
}
