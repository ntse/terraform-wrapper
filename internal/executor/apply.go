package executor

import (
	"context"
	"fmt"
	"path/filepath"

	"terraform-wrapper/internal/graph"
	"terraform-wrapper/internal/output"
	"terraform-wrapper/internal/stacks"
)

func ApplyAll(ctx context.Context, g graph.Graph, opts Options) (*Summary, error) {
	opts.UseCache = false
	return RunAll(ctx, g, opts, OperationApply)
}

func DestroyAll(ctx context.Context, g graph.Graph, opts Options) (*Summary, error) {
	opts.UseCache = false
	return RunAll(ctx, g, opts, OperationDestroy)
}

func InitAll(ctx context.Context, g graph.Graph, opts Options) (*Summary, error) {
	opts.UseCache = false
	return RunAll(ctx, g, opts, OperationInit)
}

func ApplyStack(ctx context.Context, stack *graph.Stack, opts Options) (*Summary, error) {
	return runSingle(ctx, stack, opts, OperationApply)
}

func DestroyStack(ctx context.Context, stack *graph.Stack, opts Options) (*Summary, error) {
	return runSingle(ctx, stack, opts, OperationDestroy)
}

func InitStack(ctx context.Context, stack *graph.Stack, opts Options) (*Summary, error) {
	return runSingle(ctx, stack, opts, OperationInit)
}

func runSingle(ctx context.Context, stack *graph.Stack, opts Options, op Operation) (*Summary, error) {
	opts.Defaults()
	if opts.TerraformPath == "" {
		return nil, fmt.Errorf("terraform binary path not provided")
	}

	runner, err := newRunner(ctx, stacks.RunnerOptions{
		RootDir:        opts.RootDir,
		Environment:    opts.Environment,
		AccountID:      opts.AccountID,
		Region:         opts.Region,
		TerraformPath:  opts.TerraformPath,
		DisableRefresh: opts.DisableRefresh,
	})
	if err != nil {
		return nil, err
	}

	rootAbs, err := filepath.Abs(opts.RootDir)
	if err != nil {
		return nil, err
	}
	rel, err := filepath.Rel(rootAbs, stack.Path)
	if err != nil {
		return nil, err
	}

	progress := output.NewManager()
	progress.Register(rel)
	progress.Start(rel)

	var execErr error
	switch op {
	case OperationApply:
		execErr = runner.Apply(ctx, stack.Path)
	case OperationDestroy:
		execErr = runner.Destroy(ctx, stack.Path)
	case OperationInit:
		execErr = runner.InitOnly(ctx, stack.Path, true)
	default:
		execErr = fmt.Errorf("unknown operation")
	}

	if execErr != nil {
		progress.Fail(rel, execErr)
		return &Summary{Failed: map[string]error{rel: execErr}}, execErr
	}

	progress.Succeed(rel)
	return &Summary{Executed: 1}, nil
}
