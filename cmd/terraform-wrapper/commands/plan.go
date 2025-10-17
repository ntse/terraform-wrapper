package commands

import (
	"fmt"

	"github.com/spf13/cobra"

	"terraform-wrapper/internal/executor"
	"terraform-wrapper/internal/superplan"
)

func newPlanCommand() *cobra.Command {
	var stackArg string
	cmd := &cobra.Command{
		Use:   "plan",
		Short: "Run terraform plan for a single stack",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := contextWithCmd(cmd)
			g, index, err := loadGraphData()
			if err != nil {
				return err
			}
			stack, rel, err := resolveStackArg(g, index, stackArg)
			if err != nil {
				return err
			}

			res, err := resolveTerraform(ctx, cmd, []string{stack.Path})
			if err != nil {
				return err
			}

			resolvedVersion := ""
			if res.Version != nil {
				resolvedVersion = res.Version.String()
			}

			opts := executorOptions(res.BinaryPath, resolvedVersion)
			summary, err := executor.PlanStack(ctx, stack, opts)
			if err != nil {
				return err
			}

			printSummary("plan", summary)
			fmt.Printf("stack planned: %s\n", rel)
			return nil
		},
	}
	cmd.Flags().StringVar(&stackArg, "stack", "", "stack name or path")
	cmd.MarkFlagRequired("stack")
	return cmd
}

func newPlanAllCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "plan-all",
		Short: "Plan all stacks respecting dependencies",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := contextWithCmd(cmd)
			g, _, err := loadGraphData()
			if err != nil {
				return err
			}

			res, err := resolveTerraform(ctx, cmd, graphStackPaths(g))
			if err != nil {
				return err
			}

			resolvedVersion := ""
			if res.Version != nil {
				resolvedVersion = res.Version.String()
			}

			return superplan.Run(ctx, superplan.Options{
				RootDir:           rootDir,
				OutputDir:         superplanDir,
				TerraformPath:     res.BinaryPath,
				TerraformVersion:  resolvedVersion,
				Environment:       environment,
				AccountID:         accountID,
				Region:            region,
				KeepPlanArtifacts: keepPlanArtifacts,
			})
		},
	}
	return cmd
}
