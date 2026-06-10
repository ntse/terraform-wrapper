package commands

import (
	"fmt"

	"github.com/spf13/cobra"

	"terraform-wrapper/internal/executor"
)

func newRefreshCommand() *cobra.Command {
	var stackArg string
	cmd := &cobra.Command{
		Use:   "refresh",
		Short: "Run terraform refresh for a specific stack",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
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
			summary, err := executor.RefreshStack(ctx, stack, opts)
			if err != nil {
				return err
			}
			printSummary("refresh", summary)
			fmt.Printf("stack refreshed: %s\n", rel)
			return nil
		},
	}
	cmd.Flags().StringVar(&stackArg, "stack", "", "stack name or path")
	_ = cmd.MarkFlagRequired("stack")
	return cmd
}

func newRefreshAllCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "refresh-all",
		Short: "Refresh state for all stacks in dependency order",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := cmd.Context()
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
			summary, err := executor.RefreshAll(ctx, g, opts)
			if err != nil {
				return err
			}
			printSummary("refresh-all", summary)
			return nil
		},
	}
	return cmd
}
