package versioning

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/hashicorp/go-version"
	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/zclconf/go-cty/cty"
)

const defaultConstraint = ">= 1.0.0"

// DetectConstraints walks each stack directory, extracts terraform.required_version
// expressions, and returns the resolved constraint string keyed by relative stack path.
func DetectConstraints(root string, stackPaths []string) (map[string]string, error) {
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("resolve root path: %w", err)
	}

	result := make(map[string]string, len(stackPaths))
	for _, stack := range stackPaths {
		if stack == "" {
			continue
		}
		stackAbs, err := filepath.Abs(stack)
		if err != nil {
			return nil, fmt.Errorf("abs stack path %q: %w", stack, err)
		}

		rel, err := filepath.Rel(rootAbs, stackAbs)
		if err != nil {
			return nil, fmt.Errorf("stack path relative to root: %w", err)
		}

		constraints, err := detectStackConstraints(stackAbs)
		if err != nil {
			return nil, fmt.Errorf("detect constraints for %s: %w", rel, err)
		}

		if len(constraints) == 0 {
			result[rel] = defaultConstraint
			continue
		}

		result[rel] = strings.Join(constraints, ", ")
	}

	return result, nil
}

func detectStackConstraints(stackDir string) ([]string, error) {
	seen := make(map[string]struct{})
	var constraints []string

	err := filepath.WalkDir(stackDir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		if filepath.Ext(path) != ".tf" {
			return nil
		}

		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}

		file, diags := hclsyntax.ParseConfig(data, path, hcl.InitialPos)
		if diags.HasErrors() {
			return diags
		}

		body, ok := file.Body.(*hclsyntax.Body)
		if !ok {
			return fmt.Errorf("%s: unexpected HCL body type %T", path, file.Body)
		}

		for _, block := range body.Blocks {
			if block.Type != "terraform" || len(block.Labels) > 0 {
				continue
			}
			attr, ok := block.Body.Attributes["required_version"]
			if !ok {
				continue
			}
			val, diags := attr.Expr.Value(nil)
			if diags.HasErrors() {
				return diags
			}
			if val.Type() != cty.String {
				return fmt.Errorf("%s: required_version must be a string literal", path)
			}
			constraint := strings.TrimSpace(val.AsString())
			if constraint == "" {
				continue
			}
			if _, err := version.NewConstraint(constraint); err != nil {
				return fmt.Errorf("%s: invalid required_version %q: %w", path, constraint, err)
			}
			if _, exists := seen[constraint]; exists {
				continue
			}
			seen[constraint] = struct{}{}
			constraints = append(constraints, constraint)
		}

		return nil
	})

	return constraints, err
}
