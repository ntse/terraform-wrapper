package commands

import (
	"path/filepath"

	"github.com/spf13/cobra"

	"terraform-wrapper/internal/bootstrap"
)

func newBootstrapCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "bootstrap",
		Short: "Bootstrap backend infrastructure",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx := contextWithCmd(cmd)
			g, _, err := loadGraphData()
			if err != nil {
				return err
			}

			paths := graphStackPaths(g)
			if len(paths) == 0 {
				paths = defaultBootstrapStacks()
			}

			res, err := resolveTerraform(ctx, cmd, paths)
			if err != nil {
				return err
			}

			return bootstrap.Run(ctx, bootstrap.Options{
				RootDir:       rootDir,
				TerraformPath: res.BinaryPath,
				Environment:   environment,
				AccountID:     accountID,
				Region:        region,
			})
		},
	}
	return cmd
}

func defaultBootstrapStacks() []string {
	var paths []string
	bootstrapPath := filepath.Join(rootDir, "core-services", "bootstrap")
	if abs, err := filepath.Abs(bootstrapPath); err == nil {
		paths = append(paths, abs)
	}
	if len(paths) == 0 {
		if abs, err := filepath.Abs(rootDir); err == nil {
			paths = append(paths, abs)
		}
	}
	return paths
}
