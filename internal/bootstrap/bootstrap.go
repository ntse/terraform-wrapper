package bootstrap

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/hashicorp/terraform-exec/tfexec"
	"terraform-wrapper/internal/awsaccount"
	"terraform-wrapper/internal/stacks"
)

type Options struct {
	RootDir       string
	TerraformPath string
	Environment   string
	AccountID     string
	Region        string
}

func (o *Options) applyDefaults() {
	if o.RootDir == "" {
		o.RootDir = "."
	}
	if o.Environment == "" {
		o.Environment = "dev"
	}
	if o.Region == "" {
		o.Region = "eu-west-2"
	}
}

func Run(ctx context.Context, opts Options) error {
	opts.applyDefaults()
	if opts.AccountID == "" {
		account, err := awsaccount.CallerAccountID(ctx, opts.Region)
		if err != nil {
			return fmt.Errorf("failed to discover AWS account ID: %w", err)
		}
		opts.AccountID = account
	}

	stateStack := filepath.Join(opts.RootDir, "core-services", "state-file")
	backendPath := filepath.Join(stateStack, "backend.tf")
	disabledBackendPath := backendPath + ".disabled"

	if _, err := os.Stat(stateStack); err != nil {
		return fmt.Errorf("state-file stack not found at %s: %w", stateStack, err)
	}

	if _, err := os.Stat(backendPath); err != nil {
		return fmt.Errorf("backend.tf not found in %s: %w", stateStack, err)
	}

	if _, err := os.Stat(disabledBackendPath); err == nil {
		return fmt.Errorf("backend already disabled at %s (found existing backend.tf.disabled)", disabledBackendPath)
	}

	if err := os.Rename(backendPath, disabledBackendPath); err != nil {
		return fmt.Errorf("failed to disable backend: %w", err)
	}

	restored := false
	defer func() {
		if !restored {
			if err := os.Rename(disabledBackendPath, backendPath); err != nil {
				fmt.Fprintf(os.Stderr, "[bootstrap] warning: failed to restore backend.tf: %v\n", err)
			}
		}
	}()

	if opts.TerraformPath == "" {
		return fmt.Errorf("terraform binary path is required")
	}

	tf, err := tfexec.NewTerraform(stateStack, opts.TerraformPath)
	if err != nil {
		return fmt.Errorf("failed to create terraform executor: %w", err)
	}
	tf.SetStdout(os.Stdout)
	tf.SetStderr(os.Stderr)

	fmt.Println("[bootstrap] Running local apply for backend creation")

	if err := tf.Init(ctx, tfexec.BackendConfig("path=terraform.tfstate")); err != nil {
		return fmt.Errorf("local init failed: %w", err)
	}

	varFiles := stacks.VarFiles(opts.RootDir, stateStack, opts.Environment)

	applyOpts := make([]tfexec.ApplyOption, 0, len(varFiles))
	for _, vf := range varFiles {
		applyOpts = append(applyOpts, tfexec.VarFile(vf))
	}

	tf.SetEnv(map[string]string{
		"TF_CLI_ARGS_apply": "-auto-approve",
	})
	if err := tf.Apply(ctx, applyOpts...); err != nil {
		return fmt.Errorf("local apply failed: %w", err)
	}
	tf.SetEnv(nil)

	bucketName, tableName := deriveBackendNames(opts)

	if outputs, err := tf.Output(ctx); err == nil {
		if val, ok := extractStringOutput(outputs, "state_bucket_name"); ok {
			bucketName = val
		}
		if val, ok := extractStringOutput(outputs, "state_bucket_id"); ok {
			bucketName = val
		}
		if val, ok := extractStringOutput(outputs, "state_lock_table_name"); ok {
			tableName = val
		}
		if val, ok := extractStringOutput(outputs, "state_dynamodb_table_name"); ok {
			tableName = val
		}
	}

	fmt.Printf("[bootstrap] Created S3 bucket: %s\n", bucketName)
	fmt.Printf("[bootstrap] Created DynamoDB table: %s\n", tableName)

	if err := os.Rename(disabledBackendPath, backendPath); err != nil {
		return fmt.Errorf("failed to restore backend: %w", err)
	}
	restored = true

	backendConfig := map[string]string{
		"bucket":         bucketName,
		"key":            fmt.Sprintf("%s/state-file/terraform.tfstate", opts.Environment),
		"region":         opts.Region,
		"dynamodb_table": tableName,
		"encrypt":        "true",
		"use_lockfile":   "true",
	}

	var initOpts []tfexec.InitOption
	for k, v := range backendConfig {
		initOpts = append(initOpts, tfexec.BackendConfig(fmt.Sprintf("%s=%s", k, v)))
	}

	fmt.Println("[bootstrap] Migrating local state to remote backend...")

	tf.SetEnv(map[string]string{
		"TF_CLI_ARGS_init": "-migrate-state",
	})
	if err := tf.Init(ctx, initOpts...); err != nil {
		tf.SetEnv(nil)
		fmt.Fprintf(os.Stderr, "[bootstrap] migration failed: %v\n", err)
		fmt.Fprintf(os.Stderr, "[bootstrap] local state remains at %s\n", filepath.Join(stateStack, "terraform.tfstate"))
		return fmt.Errorf("state migration failed: %w", err)
	}
	tf.SetEnv(nil)

	fmt.Println("[bootstrap] Backend bootstrapped")

	if err := runOIDC(ctx, opts); err != nil {
		return err
	}

	return nil
}

func deriveBackendNames(opts Options) (string, string) {
	bucket := fmt.Sprintf("%s-%s-terraform-state", opts.AccountID, opts.Region)
	table := fmt.Sprintf("%s-%s-state-locks", opts.Environment, opts.Region)
	return bucket, table
}

func extractStringOutput(outputs map[string]tfexec.OutputMeta, key string) (string, bool) {
	meta, ok := outputs[key]
	if !ok {
		return "", false
	}
	var value string
	if err := json.Unmarshal(meta.Value, &value); err != nil {
		return "", false
	}
	return value, true
}

func runOIDC(ctx context.Context, opts Options) error {
	oidcDir := filepath.Join(opts.RootDir, "core-services", "oidc")
	if info, err := os.Stat(oidcDir); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("failed to stat oidc stack: %w", err)
	} else if !info.IsDir() {
		return fmt.Errorf("oidc path %s is not a directory", oidcDir)
	}

	fmt.Println("[bootstrap] Running oidc stack apply")

	runner, err := stacks.NewRunner(ctx, stacks.RunnerOptions{
		RootDir:       opts.RootDir,
		Environment:   opts.Environment,
		AccountID:     opts.AccountID,
		Region:        opts.Region,
		TerraformPath: opts.TerraformPath,
	})
	if err != nil {
		return fmt.Errorf("failed to create runner for oidc: %w", err)
	}

	if err := runner.Apply(ctx, oidcDir); err != nil {
		return fmt.Errorf("oidc apply failed: %w", err)
	}

	return nil
}
