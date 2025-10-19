package commands

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/spf13/cobra"

	"terraform-wrapper/internal/graph"
)

func newCleanCommand() *cobra.Command {
	var stackArg string
	cmd := &cobra.Command{
		Use:   "clean",
		Short: "Remove .terraform artifacts for a specific stack",
		RunE: func(cmd *cobra.Command, args []string) error {
			g, index, err := loadGraphData()
			if err != nil {
				return err
			}

			stack, rel, err := resolveStackArg(g, index, stackArg)
			if err != nil {
				return err
			}

			if err := cleanStackArtifacts(stack.Path); err != nil {
				return err
			}

			fmt.Printf("[clean] removed .terraform artifacts for %s\n", rel)
			return nil
		},
	}

	cmd.Flags().StringVar(&stackArg, "stack", "", "stack name or path")
	_ = cmd.MarkFlagRequired("stack")
	return cmd
}

func newCleanAllCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "clean-all",
		Short: "Remove .terraform artifacts for every stack",
		RunE: func(cmd *cobra.Command, args []string) error {
			g, _, err := loadGraphData()
			if err != nil {
				return err
			}

			stacks := make([]*graph.Stack, 0, len(g))
			for _, stack := range g {
				stacks = append(stacks, stack)
			}

			if err := cleanStacks(stacks); err != nil {
				return err
			}

			for _, stack := range stacks {
				rel, err := filepathRelSafe(rootDir, stack.Path)
				if err != nil {
					rel = stack.Path
				}
				fmt.Printf("[clean] removed .terraform artifacts for %s\n", rel)
			}

			return nil
		},
	}

	return cmd
}

func cleanStacks(stacks []*graph.Stack) error {
	for _, stack := range stacks {
		if err := cleanStackArtifacts(stack.Path); err != nil {
			return err
		}
	}
	return nil
}

func cleanStackArtifacts(stackPath string) error {
	terraformDir := filepath.Join(stackPath, ".terraform")
	if err := os.RemoveAll(terraformDir); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove %s: %w", terraformDir, err)
	}

	for _, lockFile := range []string{"terraform.lock.hcl", ".terraform.lock.hcl"} {
		lockPath := filepath.Join(stackPath, lockFile)
		if err := os.Remove(lockPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove %s: %w", lockPath, err)
		}
	}

	return nil
}
