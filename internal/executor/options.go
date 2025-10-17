package executor

import (
	"context"
	"path/filepath"
)

type Operation int

const (
	OperationInit Operation = iota
	OperationPlan
	OperationApply
	OperationDestroy
)

type runner interface {
	Apply(context.Context, string) error
	Destroy(context.Context, string) error
	InitOnly(context.Context, string, bool) error
	PlanWithOutput(context.Context, string, string) error
	VarFilesFor(string) []string
}

type Options struct {
	RootDir          string
	Environment      string
	AccountID        string
	Region           string
	TerraformPath    string
	TerraformVersion string
	Parallelism      int
	UseCache         bool
	ForceStacks      map[string]struct{}
	DisableRefresh   bool
}

func (o *Options) Defaults() {
	if o.RootDir == "" {
		o.RootDir = "."
	}
	if o.Environment == "" {
		o.Environment = "dev"
	}
	if o.Region == "" {
		o.Region = "eu-west-2"
	}
	if o.TerraformPath == "" {
		o.TerraformPath = "terraform"
	}
	if o.Parallelism <= 0 {
		o.Parallelism = 4
	}
}

func (o *Options) Relative(path string) (string, error) {
	rootAbs, err := filepath.Abs(o.RootDir)
	if err != nil {
		return "", err
	}
	stackAbs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	return filepath.Rel(rootAbs, stackAbs)
}

func (o *Options) IsForced(stackRel string) bool {
	if o.ForceStacks == nil {
		return false
	}
	_, ok := o.ForceStacks[stackRel]
	return ok
}
