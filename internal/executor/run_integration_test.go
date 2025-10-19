package executor

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/hashicorp/terraform-exec/tfexec"
	"github.com/stretchr/testify/require"

	"terraform-wrapper/internal/graph"
	"terraform-wrapper/internal/stacks"
)

const exampleProjectRepo = "https://github.com/ukhsa-collaboration/devops-terraform-example-project"

func TestRunAllInitIntegration(t *testing.T) {
	if testing.Short() {
		t.Skip("integration test")
	}

	requireBinary(t, "git")
	tfPath := requireBinary(t, "terraform")

	root := cloneExampleProject(t)

	g, err := graph.Build(root)
	require.NoError(t, err)

	network := filepath.Join(root, "core-services", "network")
	ecs := filepath.Join(root, "core-services", "ecs")
	app := filepath.Join(root, "applications", "containers")

	selected := graph.Graph{
		network: g[network],
		ecs:     g[ecs],
		app:     g[app],
	}

	withIntegrationRunner(t)

	opts := Options{
		RootDir:       root,
		Environment:   "dev",
		AccountID:     "000000000000",
		Region:        "eu-west-2",
		TerraformPath: tfPath,
		Parallelism:   1,
	}

	ctx := context.Background()
	summary, err := RunAll(ctx, selected, opts, OperationInit)
	require.NoError(t, err)
	require.Nil(t, summary.Failed)
	require.Equal(t, len(selected), summary.Executed)
}

func requireBinary(t *testing.T, name string) string {
	t.Helper()
	path, err := exec.LookPath(name)
	if err != nil {
		t.Skipf("%s not available on %s: %v", name, runtime.GOOS, err)
	}
	return path
}

func cloneExampleProject(t *testing.T) string {
	t.Helper()

	dest := filepath.Join(t.TempDir(), "devops-terraform-example-project")
	cmd := exec.Command("git", "clone", "--depth", "1", exampleProjectRepo, dest)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git clone failed: %v\n%s", err, string(output))
	}
	return dest
}

func withIntegrationRunner(t *testing.T) {
	origRunner := newRunner
	newRunner = func(ctx context.Context, opts stacks.RunnerOptions) (runner, error) {
		return &integrationRunner{
			root:          opts.RootDir,
			environment:   opts.Environment,
			terraformPath: opts.TerraformPath,
		}, nil
	}
	t.Cleanup(func() {
		newRunner = origRunner
	})
}

type integrationRunner struct {
	root          string
	environment   string
	terraformPath string
}

func (r *integrationRunner) Apply(context.Context, string) error {
	// TODO: implement apply intergration test against Localstack
	return errors.New("apply not supported in integration runner")
}

func (r *integrationRunner) Destroy(context.Context, string) error {
	// TODO: implement destroy intergration test against Localstack
	return errors.New("destroy not supported in integration runner")
}

func (r *integrationRunner) InitOnly(ctx context.Context, stack string, upgrade bool) error {
	tf, err := r.newTerraform(stack)
	if err != nil {
		return err
	}

	initOpts := []tfexec.InitOption{tfexec.Backend(false)}
	if upgrade {
		initOpts = append(initOpts, tfexec.Upgrade(true))
	}
	return tf.Init(ctx, initOpts...)
}

func (r *integrationRunner) PlanWithOutput(ctx context.Context, stack, planPath string) error {
	tf, err := r.newTerraform(stack)
	if err != nil {
		return err
	}

	if err := tf.Init(ctx, tfexec.Backend(false)); err != nil {
		return err
	}

	opts := []tfexec.PlanOption{tfexec.Out(planPath), tfexec.Lock(false), tfexec.Refresh(false)}
	for _, vf := range r.VarFilesFor(stack) {
		opts = append(opts, tfexec.VarFile(vf))
	}

	_, err = tf.Plan(ctx, opts...)
	return err
}

func (r *integrationRunner) VarFilesFor(stack string) []string {
	return stacks.VarFiles(r.root, stack, r.environment)
}

func (r *integrationRunner) newTerraform(stack string) (*tfexec.Terraform, error) {
	tf, err := tfexec.NewTerraform(stack, r.terraformPath)
	if err != nil {
		return nil, err
	}
	tf.SetStdout(io.Discard)
	tf.SetStderr(io.Discard)
	return tf, nil
}
