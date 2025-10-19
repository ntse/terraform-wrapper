package executor

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"terraform-wrapper/internal/cache"
	"terraform-wrapper/internal/graph"
	"terraform-wrapper/internal/output"
	"terraform-wrapper/internal/stacks"
)

type ResultStatus int

const (
	StatusExecuted ResultStatus = iota
	StatusCached
	StatusSkipped
)

var (
	newRunner = func(ctx context.Context, opts stacks.RunnerOptions) (runner, error) {
		return stacks.NewRunner(ctx, opts)
	}
)

type executor struct {
	ctx             context.Context
	options         Options
	graph           graph.Graph
	rootAbs         string
	terraformPath   string
	relNames        map[string]string
	indegree        map[string]int
	dependents      map[string][]string
	progress        *output.Manager
	waitingNotified map[string]bool
	planHashes      map[string][]byte
	hashMu          sync.Mutex
}

func newExecutor(ctx context.Context, g graph.Graph, opts Options) (*executor, error) {
	opts.Defaults()
	rootAbs, err := filepath.Abs(opts.RootDir)
	if err != nil {
		return nil, err
	}

	terraformPath := opts.TerraformPath
	if terraformPath == "" {
		return nil, fmt.Errorf("terraform binary path not provided")
	}

	relNames := make(map[string]string)
	indegree := make(map[string]int)
	dependents := make(map[string][]string)
	progress := output.NewManager()
	for path, stack := range g {
		rel, err := filepath.Rel(rootAbs, path)
		if err != nil {
			return nil, err
		}
		relNames[path] = rel
		progress.Register(rel)
		indegree[path] = len(stack.Dependencies)
		for _, dep := range stack.Dependencies {
			dependents[dep] = append(dependents[dep], path)
		}
	}

	return &executor{
		ctx:             ctx,
		options:         opts,
		graph:           g,
		rootAbs:         rootAbs,
		terraformPath:   terraformPath,
		relNames:        relNames,
		indegree:        indegree,
		dependents:      dependents,
		progress:        progress,
		waitingNotified: make(map[string]bool),
		planHashes:      make(map[string][]byte),
	}, nil
}

func (e *executor) readyNodes(processed map[string]bool) []string {
	var layer []string
	for path, indeg := range e.indegree {
		if processed[path] {
			continue
		}
		if indeg == 0 {
			layer = append(layer, path)
		}
	}
	return layer
}

func (e *executor) notifyWaiting(processed map[string]bool) {
	for path, indeg := range e.indegree {
		if processed[path] || indeg == 0 || e.waitingNotified[path] {
			continue
		}
		var waitingOn []string
		for _, dep := range e.graph[path].Dependencies {
			if !processed[dep] {
				waitingOn = append(waitingOn, e.relNames[dep])
			}
		}
		if len(waitingOn) == 0 {
			continue
		}
		e.waitingNotified[path] = true
		rel := e.relNames[path]
		e.progress.Waiting(rel, fmt.Sprintf("waiting for %s", strings.Join(waitingOn, ", ")))
	}
}

func RunAll(ctx context.Context, g graph.Graph, opts Options, op Operation) (*Summary, error) {
	exec, err := newExecutor(ctx, g, opts)
	if err != nil {
		return nil, err
	}

	summary := &Summary{}
	processed := make(map[string]bool)
	layerIndex := 1

	for len(processed) < len(g) {
		exec.notifyWaiting(processed)
		layer := exec.readyNodes(processed)
		if len(layer) == 0 {
			return summary, errors.New("dependency cycle detected")
		}

		fmt.Printf("[layer %d] running: %s\n", layerIndex, exec.layerNames(layer))
		layerSummary, err := exec.runLayer(layer, op)
		summary.Merge(layerSummary)
		if err != nil {
			return summary, err
		}

		for _, node := range layer {
			processed[node] = true
			for _, dep := range exec.dependents[node] {
				exec.indegree[dep]--
			}
		}
		layerIndex++
	}

	return summary, nil
}

func (e *executor) layerNames(layer []string) string {
	rels := make([]string, len(layer))
	for i, path := range layer {
		rels[i] = e.relNames[path]
	}
	return strings.Join(rels, ", ")
}

func (e *executor) runLayer(layer []string, op Operation) (Summary, error) {
	ctx, cancel := context.WithCancel(e.ctx)
	defer cancel()

	sem := make(chan struct{}, e.options.Parallelism)
	var wg sync.WaitGroup
	var mu sync.Mutex
	var firstErr error
	summary := Summary{Failed: make(map[string]error)}

	for _, stackPath := range layer {
		// looks like an error, not an error! shadow loop variable so each goroutine gets its own copy.
		stackPath := stackPath
		rel := e.relNames[stackPath]
		stack := e.graph[stackPath]
		wg.Add(1)
		go func(rel string, stack *graph.Stack) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-sem }()

			e.progress.Start(rel)

			status, err := e.executeStack(ctx, stack, rel, op)

			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				e.progress.Fail(rel, err)
				summary.Failed[rel] = err
				if firstErr == nil {
					firstErr = err
					cancel()
				}
				return
			}
			switch status {
			case StatusCached:
				e.progress.Skip(rel, "cache hit")
				summary.Cached++
			case StatusSkipped:
				e.progress.Skip(rel, "skipped")
				summary.Skipped++
			default:
				e.progress.Succeed(rel)
				summary.Executed++
			}
		}(rel, stack)
	}

	wg.Wait()
	if len(summary.Failed) == 0 {
		summary.Failed = nil
	}
	return summary, firstErr
}

func (e *executor) executeStack(ctx context.Context, stack *graph.Stack, rel string, op Operation) (ResultStatus, error) {
	runner, err := newRunner(ctx, stacks.RunnerOptions{
		RootDir:        e.options.RootDir,
		Environment:    e.options.Environment,
		AccountID:      e.options.AccountID,
		Region:         e.options.Region,
		TerraformPath:  e.terraformPath,
		DisableRefresh: e.options.DisableRefresh,
	})
	if err != nil {
		return StatusExecuted, err
	}

	switch op {
	case OperationPlan:
		return e.planStack(ctx, runner, stack, rel)
	case OperationApply:
		return StatusExecuted, runner.Apply(ctx, stack.Path)
	case OperationDestroy:
		return StatusExecuted, runner.Destroy(ctx, stack.Path)
	case OperationInit:
		return StatusExecuted, runner.InitOnly(ctx, stack.Path, true)
	default:
		return StatusExecuted, fmt.Errorf("unknown operation")
	}
}

func (e *executor) planStack(ctx context.Context, runner runner, stack *graph.Stack, rel string) (ResultStatus, error) {
	stackDir := stack.Path
	varFiles := runner.VarFilesFor(stackDir)

	contentFiles, err := cache.StackContentFiles(stackDir, varFiles)
	if err != nil {
		return StatusExecuted, err
	}

	baseHash, err := cache.ComputeHash(contentFiles)
	if err != nil {
		return StatusExecuted, err
	}

	hasher := sha256.New()
	hasher.Write(baseHash)
	for _, dep := range stack.Dependencies {
		if depHash := e.getPlanHash(dep); depHash != nil {
			hasher.Write(depHash)
		}
	}
	hashBytes := hasher.Sum(nil)

	planPath, hashPath := cache.PlanFiles(e.options.RootDir, e.options.Environment, rel)

	if e.options.UseCache && !e.options.IsForced(rel) {
		if cachedHash, err := cache.LoadHash(hashPath); err == nil {
			if bytes.Equal(cachedHash, hashBytes) {
				if _, err := os.Stat(planPath); err == nil {
					e.setPlanHash(stack.Path, cachedHash)
					return StatusCached, nil
				}
			}
		}
	}

	if err := ensureDir(filepath.Dir(planPath)); err != nil {
		return StatusExecuted, err
	}

	if err := runner.PlanWithOutput(ctx, stackDir, planPath); err != nil {
		return StatusExecuted, err
	}

	if err := cache.SaveHash(hashPath, hashBytes); err != nil {
		return StatusExecuted, err
	}
	e.setPlanHash(stack.Path, hashBytes)
	return StatusExecuted, nil
}

func (e *executor) getPlanHash(stackPath string) []byte {
	e.hashMu.Lock()
	defer e.hashMu.Unlock()
	return e.planHashes[stackPath]
}

func (e *executor) setPlanHash(stackPath string, hash []byte) {
	e.hashMu.Lock()
	defer e.hashMu.Unlock()
	e.planHashes[stackPath] = hash
}

func ensureDir(path string) error {
	return os.MkdirAll(path, 0o755)
}
