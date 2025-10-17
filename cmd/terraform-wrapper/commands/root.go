package commands

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/hashicorp/go-version"
	"github.com/spf13/cobra"

	"terraform-wrapper/internal/awsaccount"
	"terraform-wrapper/internal/executor"
	"terraform-wrapper/internal/graph"
	"terraform-wrapper/internal/versioning"
)

var (
	rootDir           string
	environment       string
	envAlias          string
	terraformVersion  string
	accountID         string
	region            string
	superplanDir      string
	parallelism       int
	cacheEnabled      bool
	forcePlanStacks   []string
	keepPlanArtifacts bool
	refreshState      bool
)

var wrapperVersion = "dev-1"

var rootCmd = &cobra.Command{
	Use:     "terraform-wrapper",
	Short:   "Terraform orchestration toolkit",
	Version: wrapperVersion,
	PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
		if envAlias != "" {
			environment = envAlias
		}
		if environment == "" {
			return fmt.Errorf("environment must be specified via --environment or --env")
		}
		if parallelism <= 0 {
			parallelism = 4
		}
		if accountID == "" {
			ctx := cmd.Context()
			id, err := awsaccount.CallerAccountID(ctx, region)
			if err != nil {
				return err
			}
			accountID = id
		}
		return nil
	},
}

func init() {
	rootCmd.SetVersionTemplate("terraform-wrapper version {{.Version}}\n")
	rootCmd.PersistentFlags().StringVar(&rootDir, "root", ".", "root directory containing Terraform stacks")
	rootCmd.PersistentFlags().StringVar(&terraformVersion, "terraform-version", "", "Optional exact Terraform version to enforce")
	rootCmd.PersistentFlags().StringVar(&environment, "environment", "", "environment name (required)")
	rootCmd.PersistentFlags().StringVar(&envAlias, "env", "", "environment name alias")
	rootCmd.PersistentFlags().StringVar(&accountID, "account-id", "", "AWS account ID (defaults to caller identity)")
	rootCmd.PersistentFlags().StringVar(&region, "region", "eu-west-2", "AWS region")
	rootCmd.PersistentFlags().StringVar(&superplanDir, "out", "superplan", "directory for generated superplan artifacts")
	rootCmd.PersistentFlags().IntVar(&parallelism, "parallelism", 4, "number of stacks to run concurrently")
	rootCmd.PersistentFlags().BoolVar(&cacheEnabled, "cache", true, "enable plan cache reuse")
	rootCmd.PersistentFlags().StringSliceVar(&forcePlanStacks, "force-plan", nil, "comma separated list of stacks to force planning")
	rootCmd.PersistentFlags().BoolVar(&keepPlanArtifacts, "keep-plan-artifacts", false, "preserve generated superplan artifacts")
	rootCmd.PersistentFlags().BoolVar(&refreshState, "refresh", true, "refresh state before planning")

	rootCmd.AddCommand(newBootstrapCommand())
	rootCmd.AddCommand(newPlanCommand())
	rootCmd.AddCommand(newApplyCommand())
	rootCmd.AddCommand(newDestroyCommand())
	rootCmd.AddCommand(newInitCommand())
	rootCmd.AddCommand(newPlanAllCommand())
	rootCmd.AddCommand(newApplyAllCommand())
	rootCmd.AddCommand(newDestroyAllCommand())
	rootCmd.AddCommand(newInitAllCommand())
	rootCmd.AddCommand(newCleanCommand())
	rootCmd.AddCommand(newCleanAllCommand())
}

func Execute() error {
	return rootCmd.Execute()
}

func contextWithCmd(cmd *cobra.Command) context.Context {
	return cmd.Context()
}

func displayStack(path string) string {
	if !strings.HasPrefix(path, rootDir) {
		return path
	}
	rel, err := filepathRelSafe(rootDir, path)
	if err != nil {
		return path
	}
	return rel
}

func filepathRelSafe(base, target string) (string, error) {
	baseAbs, err := filepath.Abs(base)
	if err != nil {
		return "", err
	}
	targetAbs, err := filepath.Abs(target)
	if err != nil {
		return "", err
	}
	return filepath.Rel(baseAbs, targetAbs)
}

func fatalf(format string, args ...interface{}) error {
	return fmt.Errorf(format, args...)
}

func ensureDir(path string) error {
	if err := os.MkdirAll(path, 0o755); err != nil {
		return fmt.Errorf("ensure dir %s: %w", path, err)
	}
	return nil
}

func printSummary(label string, summary *executor.Summary) {
	if summary == nil {
		return
	}
	fmt.Printf("[%s] executed=%d cached=%d skipped=%d\n", label, summary.Executed, summary.Cached, summary.Skipped)
	if len(summary.Failed) > 0 {
		fmt.Println("Failures:")
		for stack, err := range summary.Failed {
			fmt.Printf("  %s: %v\n", stack, err)
		}
	}
}

func executorOptions(binaryPath, resolvedVersion string) executor.Options {
	forceMap := make(map[string]struct{})
	for _, name := range forcePlanStacks {
		rel := normalizeStackName(name)
		if rel != "" {
			forceMap[rel] = struct{}{}
		}
	}
	return executor.Options{
		RootDir:          rootDir,
		Environment:      environment,
		AccountID:        accountID,
		Region:           region,
		TerraformPath:    binaryPath,
		TerraformVersion: resolvedVersion,
		Parallelism:      parallelism,
		UseCache:         cacheEnabled,
		ForceStacks:      forceMap,
		DisableRefresh:   !refreshState,
	}
}

func resolveTerraform(ctx context.Context, cmd *cobra.Command, stackPaths []string) (*versioning.ResolveResult, error) {
	if len(stackPaths) == 0 {
		return nil, fmt.Errorf("no stacks provided for Terraform resolution")
	}

	pinned, err := parsePinnedVersion()
	if err != nil {
		return nil, err
	}

	opts := versioning.ResolveOptions{
		RootDir:        rootDir,
		StackPaths:     stackPaths,
		Stdout:         cmd.OutOrStdout(),
		Stderr:         cmd.ErrOrStderr(),
		ForceInstall:   envBool("TFWRAPPER_FORCE_INSTALL"),
		UseSystemOnly:  envBool("TFWRAPPER_USE_SYSTEM_TERRAFORM"),
		DisableInstall: envBool("TFWRAPPER_DISABLE_INSTALL"),
		PinnedVersion:  pinned,
	}

	return versioning.ResolveTerraformBinary(ctx, opts)
}

func graphStackPaths(g graph.Graph) []string {
	paths := make([]string, 0, len(g))
	for path := range g {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	return paths
}

func parsePinnedVersion() (*version.Version, error) {
	if terraformVersion == "" {
		return nil, nil
	}
	v, err := version.NewVersion(terraformVersion)
	if err != nil {
		return nil, fmt.Errorf("invalid --terraform-version %q: %w", terraformVersion, err)
	}
	return v, nil
}

func envBool(key string) bool {
	raw, ok := os.LookupEnv(key)
	if !ok {
		return false
	}
	b, err := strconv.ParseBool(raw)
	if err != nil {
		return false
	}
	return b
}

func normalizeStackName(name string) string {
	if name == "" {
		return ""
	}
	var abs string
	if filepath.IsAbs(name) {
		abs = name
	} else {
		abs = filepath.Join(rootDir, name)
	}
	rel, err := filepathRelSafe(rootDir, abs)
	if err != nil {
		return name
	}
	return rel
}

func loadGraphData() (graph.Graph, map[string]*graph.Stack, error) {
	rootAbs, err := filepath.Abs(rootDir)
	if err != nil {
		return nil, nil, err
	}
	g, err := graph.Build(rootAbs)
	if err != nil {
		return nil, nil, err
	}
	idx := make(map[string]*graph.Stack)
	for path, stack := range g {
		rel, err := filepathRelSafe(rootDir, path)
		if err != nil {
			return nil, nil, err
		}
		idx[rel] = stack
	}
	return g, idx, nil
}

func resolveStackArg(g graph.Graph, index map[string]*graph.Stack, input string) (*graph.Stack, string, error) {
	if input == "" {
		return nil, "", fmt.Errorf("--stack is required")
	}
	if stack, ok := index[input]; ok {
		return stack, input, nil
	}
	rel := normalizeStackName(input)
	if stack, ok := index[rel]; ok {
		return stack, rel, nil
	}
	var matches []*graph.Stack
	var rels []string
	for relPath, stack := range index {
		if filepath.Base(relPath) == input {
			matches = append(matches, stack)
			rels = append(rels, relPath)
		}
	}
	if len(matches) == 1 {
		return matches[0], rels[0], nil
	}
	if len(matches) > 1 {
		return nil, "", fmt.Errorf("stack %q is ambiguous (%v)", input, rels)
	}
	return nil, "", fmt.Errorf("stack %q not found", input)
}
