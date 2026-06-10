package executor

import (
	"context"

	"terraform-wrapper/internal/graph"
)

func PlanAll(ctx context.Context, g graph.Graph, opts Options) (*Summary, error) {
	return RunAll(ctx, g, opts, OperationPlan)
}

func PlanStack(ctx context.Context, stack *graph.Stack, opts Options) (*Summary, error) {
	return runSingle(ctx, stack, opts, OperationPlan)
}
