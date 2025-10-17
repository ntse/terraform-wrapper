package graph

import (
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

type Stack struct {
	Path         string
	Dependencies []string
	SkipDestroy  bool
}

type Graph map[string]*Stack

type fileDependencies struct {
	Dependencies struct {
		Paths []string `json:"paths"`
	} `json:"dependencies"`
	SkipWhenDestroying bool `json:"skip_when_destroying"`
}

func Build(root string) (Graph, error) {
	rootAbs, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}

	result := make(Graph)

	err = filepath.WalkDir(rootAbs, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}

		if d.IsDir() || filepath.Base(path) != "dependencies.json" {
			return nil
		}

		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}

		var deps fileDependencies
		if err := json.Unmarshal(data, &deps); err != nil {
			return fmt.Errorf("invalid JSON in %s: %w", path, err)
		}

		stackDir := filepath.Dir(path)
		stackDirAbs, err := filepath.Abs(stackDir)
		if err != nil {
			return err
		}

		stack := ensureStack(result, stackDirAbs)
		stack.SkipDestroy = deps.SkipWhenDestroying

		for _, dep := range deps.Dependencies.Paths {
			depPath := dep
			if !filepath.IsAbs(depPath) {
				depPath = filepath.Join(rootAbs, depPath)
			}
			depAbs, err := filepath.Abs(depPath)
			if err != nil {
				return err
			}
			stack.Dependencies = append(stack.Dependencies, depAbs)
			ensureStack(result, depAbs)
		}

		return nil
	})

	return result, err
}

func ensureStack(g Graph, path string) *Stack {
	if stack, ok := g[path]; ok {
		return stack
	}
	stack := &Stack{Path: path}
	g[path] = stack
	return stack
}

func TopoSort(g Graph) ([]string, error) {
	visited := make(map[string]bool)
	tempMark := make(map[string]bool)
	var order []string

	var visit func(string) error
	visit = func(node string) error {
		if tempMark[node] {
			return fmt.Errorf("cycle detected involving %s", node)
		}
		if !visited[node] {
			tempMark[node] = true
			stack, ok := g[node]
			if !ok {
				stack = ensureStack(g, node)
			}
			for _, dep := range stack.Dependencies {
				if err := visit(dep); err != nil {
					return err
				}
			}
			tempMark[node] = false
			visited[node] = true
			order = append(order, node)
		}
		return nil
	}

	for node := range g {
		if !visited[node] {
			if err := visit(node); err != nil {
				return nil, err
			}
		}
	}

	return order, nil
}
