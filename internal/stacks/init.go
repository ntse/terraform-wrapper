package stacks

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/hashicorp/terraform-exec/tfexec"
)

type InitOptions struct {
	RootDir       string
	TerraformPath string
	Environment   string
	AccountID     string
	Region        string
	Upgrade       bool
}

func Init(ctx context.Context, stackDir string, opts InitOptions) error {
	runnerOpts := RunnerOptions{
		RootDir:       optionOrDefault(opts.RootDir, "."),
		TerraformPath: optionOrDefault(opts.TerraformPath, "terraform"),
		Environment:   optionOrDefault(opts.Environment, "dev"),
		AccountID:     optionOrDefault(opts.AccountID, "636728427214"),
		Region:        optionOrDefault(opts.Region, "eu-west-2"),
	}

	runner, err := NewRunner(ctx, runnerOpts)
	if err != nil {
		return err
	}

	stackAbs, err := filepath.Abs(stackDir)
	if err != nil {
		return err
	}

	tf, err := runner.newTerraform(stackAbs)
	if err != nil {
		return err
	}

	backend := runner.backendConfig(stackAbs)

	var initOpts []tfexec.InitOption
	for k, v := range backend {
		initOpts = append(initOpts, tfexec.BackendConfig(fmt.Sprintf("%s=%s", k, v)))
	}
	if opts.Upgrade {
		initOpts = append([]tfexec.InitOption{tfexec.Upgrade(true)}, initOpts...)
	}

	varFiles := runner.varFiles(stackAbs)
	cliArgs := varFileArgs("init", varFiles)
	if cliArgs != "" {
		tf.SetEnv(map[string]string{
			"TF_CLI_ARGS_init": cliArgs,
		})
	}
	defer tf.SetEnv(nil)

	return tf.Init(ctx, initOpts...)
}

func varFileArgs(command string, files []string) string {
	if len(files) == 0 {
		return ""
	}

	args := ""
	for _, file := range files {
		args += fmt.Sprintf(" -var-file=%s", file)
	}

	return args
}

func optionOrDefault(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}
