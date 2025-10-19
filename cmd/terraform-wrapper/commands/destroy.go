package commands

import (
	"fmt"

	"github.com/spf13/cobra"

	"terraform-wrapper/internal/executor"
)

func newDestroyCommand() *cobra.Command {
	var stackArg string
	cmd := &cobra.Command{
		Use:   "destroy",
		Short: "Run terraform destroy for a specific stack",
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
			summary, err := executor.DestroyStack(ctx, stack, opts)
			if err != nil {
				return err
			}
			printSummary("destroy", summary)
			fmt.Printf("stack destroyed: %s\n", rel)
			return nil
		},
	}
	cmd.Flags().StringVar(&stackArg, "stack", "", "stack name or path")
	_ = cmd.MarkFlagRequired("stack")
	return cmd
}

func newDestroyAllCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "destroy-all",
		Short: "Destroy all stacks in reverse dependency order",
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

			opts := executorOptions(res.BinaryPath, resolvedVersion)
			summary, err := executor.DestroyAll(ctx, g, opts)
			if err != nil {
				return err
			}
			printSummary("destroy-all", summary)
			return nil
		},
	}
	return cmd
}
