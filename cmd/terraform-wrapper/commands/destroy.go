package commands

import (
	"fmt"

	"github.com/spf13/cobra"

	"terraform-wrapper/internal/executor"
)

func newDestroyCommand() *cobra.Command {
	var stackArg string
	var autoApprove bool
	cmd := &cobra.Command{
		Use:   "destroy",
		Short: "Run terraform destroy for a specific stack",
		RunE: func(cmd *cobra.Command, args []string) error {
			if !autoApprove {
				return fmt.Errorf("destroy requires --auto-approve; pass this flag to confirm you intend to destroy resources")
			}
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
	cmd.Flags().BoolVar(&autoApprove, "auto-approve", false, "skip confirmation and destroy immediately")
	_ = cmd.MarkFlagRequired("stack")
	return cmd
}

func newDestroyAllCommand() *cobra.Command {
	var autoApprove bool
	cmd := &cobra.Command{
		Use:   "destroy-all",
		Short: "Destroy all stacks in reverse dependency order",
		RunE: func(cmd *cobra.Command, args []string) error {
			if !autoApprove {
				return fmt.Errorf("destroy-all requires --auto-approve; pass this flag to confirm you intend to destroy all resources")
			}
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
			summary, err := executor.DestroyAll(ctx, g, opts)
			if err != nil {
				return err
			}
			printSummary("destroy-all", summary)
			return nil
		},
	}
	cmd.Flags().BoolVar(&autoApprove, "auto-approve", false, "skip confirmation and destroy all stacks immediately")
	return cmd
}
