package graph_test

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/stretchr/testify/require"

	"terraform-wrapper/internal/graph"
)

func writeDependencies(t *testing.T, path string, deps []string, skip bool) {
	t.Helper()

	content := struct {
		Dependencies struct {
			Paths []string `json:"paths"`
		} `json:"dependencies"`
		SkipWhenDestroying bool `json:"skip_when_destroying"`
	}{
		SkipWhenDestroying: skip,
	}
	content.Dependencies.Paths = deps

	data, err := json.MarshalIndent(content, "", "  ")
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, data, 0o644))
}

func TestBuildGraphAndTopoSort(t *testing.T) {
	t.Parallel()

	root := t.TempDir()

	network := filepath.Join(root, "core-services", "network")
	ecs := filepath.Join(root, "core-services", "ecs")
	app := filepath.Join(root, "applications", "frontend")

	for _, dir := range []string{network, ecs, app} {
		require.NoError(t, os.MkdirAll(dir, 0o755))
	}

	writeDependencies(t, filepath.Join(network, "dependencies.json"), nil, false)
	writeDependencies(t, filepath.Join(ecs, "dependencies.json"), []string{"./core-services/network"}, false)
	writeDependencies(t, filepath.Join(app, "dependencies.json"), []string{"./core-services/ecs"}, true)

	g, err := graph.Build(root)
	require.NoError(t, err)
	require.Len(t, g, 3)

	require.Contains(t, g, absPath(t, network))
	require.Contains(t, g, absPath(t, ecs))
	require.Contains(t, g, absPath(t, app))

	require.Empty(t, g[absPath(t, network)].Dependencies)
	require.ElementsMatch(t, []string{absPath(t, network)}, g[absPath(t, ecs)].Dependencies)
	require.ElementsMatch(t, []string{absPath(t, ecs)}, g[absPath(t, app)].Dependencies)

	// Ensure SkipDestroy flag propagated.
	require.False(t, g[absPath(t, network)].SkipDestroy)
	require.True(t, g[absPath(t, app)].SkipDestroy)

	order, err := graph.TopoSort(g)
	require.NoError(t, err)

	// network must appear before ecs, which must appear before front-end.
	index := indexList(order)
	require.Less(t, index[absPath(t, network)], index[absPath(t, ecs)])
	require.Less(t, index[absPath(t, ecs)], index[absPath(t, app)])
}

func TestBuildGraphHandlesRelativePaths(t *testing.T) {
	t.Parallel()

	root := t.TempDir()
	state := filepath.Join(root, "state-file")
	extra := filepath.Join(root, "extra")

	require.NoError(t, os.MkdirAll(state, 0o755))
	require.NoError(t, os.MkdirAll(extra, 0o755))

	writeDependencies(t, filepath.Join(extra, "dependencies.json"), []string{"./state-file"}, false)
	writeDependencies(t, filepath.Join(state, "dependencies.json"), nil, false)

	g, err := graph.Build(root)
	require.NoError(t, err)

	require.ElementsMatch(t, []string{absPath(t, state)}, g[absPath(t, extra)].Dependencies)
}

func TestTopoSortDetectsCycle(t *testing.T) {
	t.Parallel()

	a := absPath(t, filepath.Join(t.TempDir(), "a"))
	b := absPath(t, filepath.Join(t.TempDir(), "b"))

	cyclic := graph.Graph{
		a: {Path: a, Dependencies: []string{b}},
		b: {Path: b, Dependencies: []string{a}},
	}

	_, err := graph.TopoSort(cyclic)
	require.Error(t, err)
	require.Contains(t, err.Error(), "cycle")
}

func TestTopoSortStableOrderForIndependentNodes(t *testing.T) {
	t.Parallel()

	a := absPath(t, filepath.Join(t.TempDir(), "a"))
	b := absPath(t, filepath.Join(t.TempDir(), "b"))
	c := absPath(t, filepath.Join(t.TempDir(), "c"))

	g := graph.Graph{
		a: {Path: a},
		b: {Path: b, Dependencies: []string{a}},
		c: {Path: c},
	}

	order, err := graph.TopoSort(g)
	require.NoError(t, err)

	index := indexList(order)
	require.Less(t, index[a], index[b], "dependency should come first")

	// Independent nodes should exist in consistent order after sorting slice copy.
	independent := []string{}
	for _, node := range order {
		if len(g[node].Dependencies) == 0 {
			independent = append(independent, node)
		}
	}

	sorted := append([]string(nil), independent...)
	sort.Strings(sorted)
	require.Equal(t, sorted, independent)
}

func absPath(t *testing.T, path string) string {
	t.Helper()
	abs, err := filepath.Abs(path)
	require.NoError(t, err)
	return abs
}

func indexList(items []string) map[string]int {
	result := make(map[string]int, len(items))
	for idx, item := range items {
		result[item] = idx
	}
	return result
}
