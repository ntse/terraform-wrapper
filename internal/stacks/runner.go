package stacks

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/hashicorp/terraform-exec/tfexec"
)

type Runner struct {
	terraformPath  string
	root           string
	environment    string
	accountID      string
	region         string
	disableRefresh bool
}

type RunnerOptions struct {
	RootDir        string
	Environment    string
	AccountID      string
	Region         string
	TerraformPath  string
	DisableRefresh bool
}

func NewRunner(ctx context.Context, opts RunnerOptions) (*Runner, error) {
	if opts.RootDir == "" {
		opts.RootDir = "."
	}
	if opts.Environment == "" {
		opts.Environment = "dev"
	}
	if opts.Region == "" {
		opts.Region = "eu-west-2"
	}
	rootAbs, err := filepath.Abs(opts.RootDir)
	if err != nil {
		return nil, err
	}
	if opts.AccountID == "" {
		return nil, fmt.Errorf("account ID is required")
	}

	if opts.TerraformPath == "" {
		return nil, fmt.Errorf("terraform binary path is required")
	}

	return &Runner{
		terraformPath:  opts.TerraformPath,
		root:           rootAbs,
		environment:    opts.Environment,
		accountID:      opts.AccountID,
		region:         opts.Region,
		disableRefresh: opts.DisableRefresh,
	}, nil
}

func (r *Runner) Plan(ctx context.Context, stackDir string) error {
	tf, err := r.newTerraform(stackDir)
	if err != nil {
		return err
	}

	if err := r.init(ctx, tf, stackDir, true); err != nil {
		return err
	}

	_, err = tf.Plan(ctx, r.planOptions(stackDir)...)
	return err
}

func (r *Runner) PlanWithOutput(ctx context.Context, stackDir, planPath string) error {
	tf, err := r.newTerraform(stackDir)
	if err != nil {
		return err
	}

	if err := r.init(ctx, tf, stackDir, true); err != nil {
		return err
	}

	planOpts := append([]tfexec.PlanOption{tfexec.Out(planPath)}, r.planOptions(stackDir)...)
	_, err = tf.Plan(ctx, planOpts...)
	return err
}

func (r *Runner) Apply(ctx context.Context, stackDir string) error {
	tf, err := r.newTerraform(stackDir)
	if err != nil {
		return err
	}

	if err := r.init(ctx, tf, stackDir, true); err != nil {
		return err
	}

	tf.SetEnv(map[string]string{
		"TF_CLI_ARGS_apply": "-auto-approve",
	})

	return tf.Apply(ctx, r.applyOptions(stackDir)...)
}

func (r *Runner) Destroy(ctx context.Context, stackDir string) error {
	tf, err := r.newTerraform(stackDir)
	if err != nil {
		return err
	}

	if err := r.init(ctx, tf, stackDir, true); err != nil {
		return err
	}

	tf.SetEnv(map[string]string{
		"TF_CLI_ARGS_destroy": "-auto-approve",
	})

	return tf.Destroy(ctx, r.destroyOptions(stackDir)...)
}

func (r *Runner) newTerraform(stackDir string) (*tfexec.Terraform, error) {
	tf, err := tfexec.NewTerraform(stackDir, r.terraformPath)
	if err != nil {
		return nil, err
	}

	tf.SetStdout(os.Stdout)
	tf.SetStderr(os.Stderr)

	return tf, nil
}

func (r *Runner) InitOnly(ctx context.Context, stackDir string, upgrade bool) error {
	tf, err := r.newTerraform(stackDir)
	if err != nil {
		return err
	}
	return r.init(ctx, tf, stackDir, upgrade)
}

func (r *Runner) init(ctx context.Context, tf *tfexec.Terraform, stackDir string, upgrade bool) error {
	backendConfig := r.backendConfig(stackDir)

	var opts []tfexec.InitOption
	for k, v := range backendConfig {
		opts = append(opts, tfexec.BackendConfig(fmt.Sprintf("%s=%s", k, v)))
	}

	if upgrade {
		opts = append([]tfexec.InitOption{tfexec.Upgrade(true)}, opts...)
	}

	return tf.Init(ctx, opts...)
}

func (r *Runner) planOptions(stackDir string) []tfexec.PlanOption {
	var opts []tfexec.PlanOption
	if r.disableRefresh {
		opts = append(opts, tfexec.Refresh(false))
	}
	for _, vf := range r.varFiles(stackDir) {
		opts = append(opts, tfexec.VarFile(vf))
	}
	return opts
}

func (r *Runner) applyOptions(stackDir string) []tfexec.ApplyOption {
	var opts []tfexec.ApplyOption
	for _, vf := range r.varFiles(stackDir) {
		opts = append(opts, tfexec.VarFile(vf))
	}
	return opts
}

func (r *Runner) destroyOptions(stackDir string) []tfexec.DestroyOption {
	var opts []tfexec.DestroyOption
	for _, vf := range r.varFiles(stackDir) {
		opts = append(opts, tfexec.VarFile(vf))
	}
	return opts
}

func (r *Runner) backendConfig(stackDir string) map[string]string {
	stackName := filepath.Base(stackDir)
	keyParts := []string{r.environment, stackName, "terraform.tfstate"}
	stateKey := strings.Join(keyParts, "/")
	bucket := fmt.Sprintf("%s-%s-state", r.accountID, r.region)

	return map[string]string{
		"bucket":  bucket,
		"key":     stateKey,
		"region":  r.region,
		"encrypt": "true",
	}
}

func (r *Runner) varFiles(stackDir string) []string {
	return VarFiles(r.root, stackDir, r.environment)
}

func (r *Runner) BackendConfig(stackDir string) map[string]string {
	return r.backendConfig(stackDir)
}

func (r *Runner) VarFilesFor(stackDir string) []string {
	return r.varFiles(stackDir)
}

func VarFiles(root, stackDir, environment string) []string {
	var files []string

	global := filepath.Join(root, "globals.tfvars")
	if fileExists(global) {
		files = append(files, global)
	}

	envFile := filepath.Join(root, "environment", fmt.Sprintf("%s.tfvars", environment))
	if fileExists(envFile) {
		files = append(files, envFile)
	}

	stackFile := filepath.Join(stackDir, "tfvars", fmt.Sprintf("%s.tfvars", environment))
	if fileExists(stackFile) {
		files = append(files, stackFile)
	}

	return files
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	return !info.IsDir()
}
