package bootstrap

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
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

	rootAbs, err := filepath.Abs(opts.RootDir)
	if err != nil {
		return fmt.Errorf("resolve root directory: %w", err)
	}

	stateStack := filepath.Join(rootAbs, "core-services", "bootstrap")
	backendPath := filepath.Join(stateStack, "backend.tf")
	disabledBackendPath := backendPath + ".disabled"

	if _, err := os.Stat(stateStack); err != nil {
		return fmt.Errorf("bootstrap stack not found at %s: %w", stateStack, err)
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

	if err := tf.Init(ctx, tfexec.Backend(false)); err != nil {
		return fmt.Errorf("local init failed: %w", err)
	}

	varFiles := stacks.VarFiles(rootAbs, stateStack, opts.Environment)

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

	bucketName := deriveBackendNames(opts)

	if outputs, err := tf.Output(ctx); err == nil {
		if val, ok := extractStringOutput(outputs, "state_bucket_name"); ok {
			bucketName = val
		}
		if val, ok := extractStringOutput(outputs, "state_bucket_id"); ok {
			bucketName = val
		}
	}

	fmt.Printf("[bootstrap] Waiting for S3 bucket %s to become available...\n", bucketName)
	if err := waitForS3Bucket(ctx, bucketName, opts.Region); err != nil {
		return fmt.Errorf("wait for S3 bucket %s: %w", bucketName, err)
	}
	fmt.Printf("[bootstrap] Bucket %s is ready\n", bucketName)

	fmt.Printf("[bootstrap] Created S3 bucket: %s\n", bucketName)

	if err := os.Rename(disabledBackendPath, backendPath); err != nil {
		return fmt.Errorf("failed to restore backend: %w", err)
	}
	restored = true

	backendConfig := map[string]string{
		"bucket":       bucketName,
		"key":          fmt.Sprintf("%s/bootstrap/terraform.tfstate", opts.Environment),
		"region":       opts.Region,
		"encrypt":      "true",
		"use_lockfile": "true",
	}

	var initOpts []tfexec.InitOption
	for k, v := range backendConfig {
		initOpts = append(initOpts, tfexec.BackendConfig(fmt.Sprintf("%s=%s", k, v)))
	}
	initOpts = append(initOpts, tfexec.ForceCopy(true))

	fmt.Println("[bootstrap] Migrating local state to remote backend...")

	if err := tf.Init(ctx, initOpts...); err != nil {
		fmt.Fprintf(os.Stderr, "[bootstrap] migration failed: %v\n", err)
		fmt.Fprintf(os.Stderr, "[bootstrap] local state remains at %s\n", filepath.Join(stateStack, "terraform.tfstate"))
		return fmt.Errorf("state migration failed: %w", err)
	}

	fmt.Println("[bootstrap] Backend bootstrapped")

	return nil
}

func deriveBackendNames(opts Options) string {
	bucket := fmt.Sprintf("%s-%s-state", opts.AccountID, opts.Region)
	return bucket
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

func waitForS3Bucket(ctx context.Context, bucket, region string) error {
	if bucket == "" {
		return fmt.Errorf("bucket name is empty")
	}
	if region == "" {
		region = "eu-west-2"
	}

	cfg, err := config.LoadDefaultConfig(ctx, config.WithRegion(region))
	if err != nil {
		return fmt.Errorf("load AWS config: %w", err)
	}

	client := s3.NewFromConfig(cfg)
	timeoutCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	var lastErr error
	for {
		callCtx, callCancel := context.WithTimeout(timeoutCtx, 10*time.Second)
		_, err := client.HeadBucket(callCtx, &s3.HeadBucketInput{Bucket: aws.String(bucket)})
		callCancel()
		if err == nil {
			return nil
		}
		lastErr = err
		select {
		case <-timeoutCtx.Done():
			if lastErr != nil {
				return fmt.Errorf("timeout waiting for bucket %s: %w (last error: %v)", bucket, timeoutCtx.Err(), lastErr)
			}
			return fmt.Errorf("timeout waiting for bucket %s: %w", bucket, timeoutCtx.Err())
		case <-ticker.C:
		}
	}
}
