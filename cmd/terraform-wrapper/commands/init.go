package commands

import (
	"fmt"

	"github.com/spf13/cobra"

	"terraform-wrapper/internal/executor"
)

func newInitCommand() *cobra.Command {
	var stackArg string
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Run terraform init for a specific stack",
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
			summary, err := executor.InitStack(ctx, stack, opts)
			if err != nil {
				return err
			}
			printSummary("init", summary)
			fmt.Printf("stack initialised: %s\n", rel)
			return nil
		},
	}
	cmd.Flags().StringVar(&stackArg, "stack", "", "stack name or path")
	cmd.MarkFlagRequired("stack")
	return cmd
}

func newInitAllCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "init-all",
		Short: "Initialise all stacks",
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
			summary, err := executor.InitAll(ctx, g, opts)
			if err != nil {
				return err
			}
			printSummary("init-all", summary)
			return nil
		},
	}
	return cmd
}
