package executor

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"

	"terraform-wrapper/internal/cache"
	"terraform-wrapper/internal/graph"
	"terraform-wrapper/internal/output"
	"terraform-wrapper/internal/stacks"
)

func PlanAll(ctx context.Context, g graph.Graph, opts Options) (*Summary, error) {
	return RunAll(ctx, g, opts, OperationPlan)
}

func PlanStack(ctx context.Context, stack *graph.Stack, opts Options) (*Summary, error) {
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

	status, err := planSingle(ctx, runner, stack, rel, opts)
	if err != nil {
		progress.Fail(rel, err)
		return &Summary{Failed: map[string]error{rel: err}}, err
	}

	if status == StatusCached {
		progress.Skip(rel, "cache hit")
		return &Summary{Cached: 1}, nil
	}

	progress.Succeed(rel)
	return &Summary{Executed: 1}, nil
}

func planSingle(ctx context.Context, runner runner, stack *graph.Stack, rel string, opts Options) (ResultStatus, error) {
	varFiles := runner.VarFilesFor(stack.Path)
	files, err := cache.StackContentFiles(stack.Path, varFiles)
	if err != nil {
		return StatusExecuted, err
	}

	hashBytes, err := cache.ComputeHash(files)
	if err != nil {
		return StatusExecuted, err
	}

	planPath, hashPath := cache.PlanFiles(opts.RootDir, opts.Environment, rel)
	planPathAbs := planPath
	if !filepath.IsAbs(planPathAbs) {
		planPathAbs, err = filepath.Abs(planPathAbs)
		if err != nil {
			return StatusExecuted, err
		}
	}

	if opts.UseCache && !opts.IsForced(rel) {
		if cachedHash, err := cache.LoadHash(hashPath); err == nil {
			if bytes.Equal(cachedHash, hashBytes) {
				if _, err := os.Stat(planPathAbs); err == nil {
					return StatusCached, nil
				}
			}
		}
	}

	if err := ensureDir(filepath.Dir(planPathAbs)); err != nil {
		return StatusExecuted, err
	}

	if err := runner.PlanWithOutput(ctx, stack.Path, planPathAbs); err != nil {
		return StatusExecuted, err
	}

	if err := cache.SaveHash(hashPath, hashBytes); err != nil {
		return StatusExecuted, err
	}

	return StatusExecuted, nil
}
