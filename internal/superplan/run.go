package superplan

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/hashicorp/terraform-exec/tfexec"
	tfjson "github.com/hashicorp/terraform-json"
	"github.com/zclconf/go-cty/cty"
	"terraform-wrapper/internal/awsaccount"
	"terraform-wrapper/internal/graph"
	"terraform-wrapper/internal/stacks"
)

type Options struct {
	RootDir           string
	OutputDir         string
	TerraformPath     string
	TerraformVersion  string
	Environment       string
	AccountID         string
	Region            string
	KeepPlanArtifacts bool
}

type stackMetadata struct {
	AbsolutePath string
	RelativePath string
	Prefix       string
}

type stackChangeSummary struct {
	Stack           string   `json:"stack"`
	Prefix          string   `json:"prefix"`
	HasChanges      bool     `json:"has_changes"`
	Adds            int      `json:"adds"`
	Changes         int      `json:"changes"`
	Destroys        int      `json:"destroys"`
	Reason          string   `json:"reason,omitempty"`
	Dependencies    []string `json:"dependencies"`
	DependentStacks []string `json:"dependent_stacks"`
}

type resourceTotals struct {
	Adds     int `json:"adds"`
	Changes  int `json:"changes"`
	Destroys int `json:"destroys"`
}

type superplanSummary struct {
	GeneratedAt       time.Time                     `json:"generated_at"`
	Environment       string                        `json:"environment"`
	AccountID         string                        `json:"account_id,omitempty"`
	TerraformVersion  string                        `json:"terraform_version"`
	TotalStacks       int                           `json:"total_stacks"`
	StacksWithChanges int                           `json:"stacks_with_changes"`
	ResourceTotals    resourceTotals                `json:"resource_totals"`
	Stacks            map[string]stackChangeSummary `json:"stacks"`
}

const planFileName = "superplan.tfplan"

func (o *Options) applyDefaults() {
	if o.RootDir == "" {
		o.RootDir = "."
	}
	if o.OutputDir == "" {
		o.OutputDir = "superplan"
	}
	if o.Environment == "" {
		o.Environment = "dev"
	}
	if o.Region == "" {
		o.Region = "eu-west-2"
	}
}

func Run(ctx context.Context, opts Options) error {
	opts.applyDefaults()

	rootAbs, err := filepath.Abs(opts.RootDir)
	if err != nil {
		return fmt.Errorf("failed to resolve root directory: %w", err)
	}
	if opts.AccountID == "" {
		account, err := awsaccount.CallerAccountID(ctx, opts.Region)
		if err != nil {
			return fmt.Errorf("failed to discover AWS account ID: %w", err)
		}
		opts.AccountID = account
	}

	stackGraph, err := graph.Build(rootAbs)
	if err != nil {
		return fmt.Errorf("error building dependency graph: %w", err)
	}

	stackInfos := make(map[string]*stackMetadata, len(stackGraph))
	stackInfosByRel := make(map[string]*stackMetadata, len(stackGraph))
	dependenciesByRel := make(map[string][]string)
	dependentsByRel := make(map[string][]string)

	order, err := graph.TopoSort(stackGraph)
	if err != nil {
		return fmt.Errorf("dependency resolution failed: %w", err)
	}
	if len(order) == 0 {
		return fmt.Errorf("no stacks discovered under %s", rootAbs)
	}

	for absPath := range stackGraph {
		rel, relErr := filepath.Rel(rootAbs, absPath)
		if relErr != nil {
			rel = absPath
		}
		rel = filepath.ToSlash(rel)
		info := &stackMetadata{
			AbsolutePath: absPath,
			RelativePath: rel,
		}
		stackInfos[absPath] = info
		stackInfosByRel[rel] = info
	}

	for absPath, stack := range stackGraph {
		info := stackInfos[absPath]
		if info == nil {
			continue
		}
		for _, depAbs := range stack.Dependencies {
			depInfo, ok := stackInfos[depAbs]
			if !ok {
				continue
			}
			dependenciesByRel[info.RelativePath] = append(dependenciesByRel[info.RelativePath], depInfo.RelativePath)
			dependentsByRel[depInfo.RelativePath] = append(dependentsByRel[depInfo.RelativePath], info.RelativePath)
		}
	}

	fmt.Printf("Discovered %d stacks\n", len(order))

	if opts.TerraformPath == "" {
		return fmt.Errorf("terraform binary path is required")
	}

	stackRunner, err := stacks.NewRunner(ctx, stacks.RunnerOptions{
		RootDir:       opts.RootDir,
		Environment:   opts.Environment,
		AccountID:     opts.AccountID,
		Region:        opts.Region,
		TerraformPath: opts.TerraformPath,
	})
	if err != nil {
		return fmt.Errorf("failed to prepare stack runner: %w", err)
	}

	var mergedResources []interface{}
	mergedOutputs := make(map[string]interface{})
	providerSources := make(map[string]string)
	stackPrefixes := make(map[string]string)
	prefixToStack := make(map[string]string)
	var baseVersion int
	var baseTFVersion string
	var serial int
	var stacksProcessed int

	for idx, stackDir := range order {
		stackName := sanitizeIdentifier(filepath.Base(stackDir))
		if stackName == "" {
			stackName = fmt.Sprintf("stack_%d", idx)
		}
		stackPrefixes[stackDir] = stackName

		if info := stackInfos[stackDir]; info != nil {
			info.Prefix = stackName
			prefixToStack[stackName] = info.RelativePath
		}

		displayName, err := filepath.Rel(rootAbs, stackDir)
		if err != nil {
			displayName = stackDir
		}

		tf, err := tfexec.NewTerraform(stackDir, opts.TerraformPath)
		if err != nil {
			return fmt.Errorf("error creating terraform executor for %s: %w", displayName, err)
		}

		backendConfig := stackRunner.BackendConfig(stackDir)

		var initOpts []tfexec.InitOption
		for k, v := range backendConfig {
			initOpts = append(initOpts, tfexec.BackendConfig(fmt.Sprintf("%s=%s", k, v)))
		}

		if err := tf.Init(ctx, initOpts...); err != nil {
			return fmt.Errorf("terraform init failed for %s: %w", displayName, err)
		}

		stateJSON, err := tf.StatePull(ctx)
		if err != nil {
			return fmt.Errorf("terraform state pull failed for %s: %w", displayName, err)
		}
		fmt.Printf("[✓] Downloaded state for stack: %s\n", displayName)

		stateMap := make(map[string]interface{})
		if err := json.Unmarshal([]byte(stateJSON), &stateMap); err != nil {
			return fmt.Errorf("invalid state file for %s: %w", displayName, err)
		}

		resCount, err := prefixResources(stateMap, stackName)
		if err != nil {
			return fmt.Errorf("failed to rewrite resources for %s: %w", displayName, err)
		}

		outCount := prefixOutputs(stateMap, stackName)

		fmt.Printf("[✓] Prefixed %d resources with '%s_'\n", resCount, stackName)
		if outCount > 0 {
			fmt.Printf("[✓] Prefixed %d outputs with '%s_'\n", outCount, stackName)
		}

		collectProviders(stateMap, providerSources)

		stripTagAttributesFromState(stateMap)

		if err := mergeState(extractResources(stateMap), extractOutputs(stateMap), &mergedResources, mergedOutputs); err != nil {
			return fmt.Errorf("failed to merge state for %s: %w", displayName, err)
		}

		if stacksProcessed == 0 {
			baseVersion = extractInt(stateMap, "version")
			baseTFVersion = extractString(stateMap, "terraform_version")
			serial = extractInt(stateMap, "serial")
		} else {
			localVersion := extractInt(stateMap, "version")
			localTFVersion := extractString(stateMap, "terraform_version")
			if localVersion != baseVersion {
				fmt.Printf("[!] Warning: %s state version %d differs from base %d\n", displayName, localVersion, baseVersion)
				if localVersion > baseVersion {
					baseVersion = localVersion
				}
			}
			if localTFVersion != "" && baseTFVersion != "" && localTFVersion != baseTFVersion {
				fmt.Printf("[!] Warning: %s Terraform version %s differs from base %s\n", displayName, localTFVersion, baseTFVersion)
			}
			if localSerial := extractInt(stateMap, "serial"); localSerial > serial {
				serial = localSerial
			}
		}
		stacksProcessed++
	}

	if serial == 0 {
		serial = int(time.Now().Unix())
	}

	lineage := fmt.Sprintf("superplan-%d", time.Now().UnixNano())

	superplanDir, err := filepath.Abs(opts.OutputDir)
	if err != nil {
		return fmt.Errorf("failed to resolve output directory: %w", err)
	}

	if err := os.MkdirAll(superplanDir, 0o755); err != nil {
		return fmt.Errorf("unable to create output directory %s: %w", superplanDir, err)
	}

	mergedDir := filepath.Join(superplanDir, "merged")
	if err := os.MkdirAll(mergedDir, 0o755); err != nil {
		return fmt.Errorf("unable to create merged configuration directory %s: %w", mergedDir, err)
	}

	stateDocument := map[string]interface{}{
		"version":           baseVersion,
		"terraform_version": baseTFVersion,
		"serial":            serial,
		"lineage":           lineage,
		"outputs":           mergedOutputs,
		"resources":         mergedResources,
	}

	statePath := filepath.Join(superplanDir, "superstate.json")
	if err := writeJSON(statePath, stateDocument); err != nil {
		return fmt.Errorf("failed to write combined state: %w", err)
	}
	fmt.Printf("[✓] Merged %d stack states into %s\n", stacksProcessed, statePath)

	configProviderRequirements, err := writeCombinedConfiguration(order, stackPrefixes, rootAbs, mergedDir)
	if err != nil {
		return fmt.Errorf("failed to build combined configuration: %w", err)
	}

	variableValues, sourcesUsed, err := collectVariableValues(rootAbs, opts.Environment, order)
	if err != nil {
		return fmt.Errorf("failed to collect variable values: %w", err)
	}

	varFilePath := filepath.Join(mergedDir, "variables.auto.tfvars")
	if err := writeTFVarsFile(varFilePath, variableValues); err != nil {
		return fmt.Errorf("failed to write variables file: %w", err)
	}
	fmt.Printf("[✓] Wrote %d variable values from %d sources to %s\n", len(variableValues), sourcesUsed, varFilePath)

	if err := ensureLocalBackend(mergedDir, providerSources, configProviderRequirements); err != nil {
		return fmt.Errorf("failed to prepare superplan configuration: %w", err)
	}

	superplanTF, err := tfexec.NewTerraform(mergedDir, opts.TerraformPath)
	if err != nil {
		return fmt.Errorf("error creating terraform executor for superplan: %w", err)
	}

	if err := superplanTF.Init(ctx, tfexec.Backend(false)); err != nil {
		return fmt.Errorf("terraform init failed in superplan directory: %w", err)
	}
	fmt.Printf("[✓] Initialized local backend in %s\n", mergedDir)

	if err := patchModuleResourceLifecycle(mergedDir); err != nil {
		return fmt.Errorf("failed to apply lifecycle ignore to modules: %w", err)
	}

	planPath := filepath.Join(superplanDir, planFileName)
	planOutRel, err := filepath.Rel(mergedDir, planPath)
	if err != nil {
		return fmt.Errorf("resolve plan output path: %w", err)
	}
	stateRel, err := filepath.Rel(mergedDir, statePath)
	if err != nil {
		return fmt.Errorf("resolve state path: %w", err)
	}

	superplanTF.SetEnv(map[string]string{
		"TF_CLI_ARGS_plan": "-input=false",
		"TF_INPUT":         "false",
	})
	defer superplanTF.SetEnv(nil)

	planHasChanges, err := superplanTF.Plan(ctx,
		tfexec.Out(planOutRel),
		tfexec.State(stateRel),
		tfexec.Refresh(false),
	)
	if err != nil {
		return fmt.Errorf("terraform plan failed: %w", err)
	}
	superplanTF.SetEnv(nil)

	fmt.Printf("[✓] Generated unified plan (%s)\n", planFileName)
	if !planHasChanges {
		fmt.Println("[i] Terraform reported no changes; summary will reflect zero-diff plan")
	}

	plan, err := superplanTF.ShowPlanFile(ctx, planPath)
	if err != nil {
		return fmt.Errorf("terraform show plan failed: %w", err)
	}

	summary := buildSuperplanSummary(plan, summaryContext{
		StackInfos:        stackInfosByRel,
		DependenciesByRel: dependenciesByRel,
		DependentsByRel:   dependentsByRel,
		PrefixToStack:     prefixToStack,
		Environment:       opts.Environment,
		AccountID:         opts.AccountID,
		TerraformVersion:  deriveTerraformVersion(opts.TerraformVersion, plan),
		GeneratedAt:       time.Now().UTC(),
	})

	summaryPath := filepath.Join(superplanDir, "superplan-summary.json")
	if err := writeJSON(summaryPath, summary); err != nil {
		return fmt.Errorf("write superplan summary: %w", err)
	}

	fmt.Printf("✅ Superplan complete: %d stacks analyzed, %d with changes\n", summary.TotalStacks, summary.StacksWithChanges)

	if err := cleanupSuperplanArtifacts(mergedDir, planPath, opts.KeepPlanArtifacts); err != nil {
		return fmt.Errorf("cleanup superplan artifacts: %w", err)
	}

	return nil
}

func prefixResources(state map[string]interface{}, stackName string) (int, error) {
	resourcesRaw, ok := state["resources"]
	if !ok {
		return 0, nil
	}

	resources, ok := resourcesRaw.([]interface{})
	if !ok {
		return 0, fmt.Errorf("unexpected resources structure: %T", resourcesRaw)
	}

	for i, r := range resources {
		resourceMap, ok := r.(map[string]interface{})
		if !ok {
			return 0, fmt.Errorf("resource %d has unexpected structure: %T", i, r)
		}

		modulePath, hasModule := resourceMap["module"].(string)

		if name, ok := resourceMap["name"].(string); ok {
			if !hasModule || modulePath == "" {
				resourceMap["name"] = prefixSegment(stackName, name)
			}
		}

		if addr, ok := resourceMap["address"].(string); ok {
			resourceMap["address"] = rewriteAddress(stackName, addr)
		}

		if moduleAddr, ok := resourceMap["module"].(string); ok && moduleAddr != "" {
			resourceMap["module"] = rewriteModuleAddress(stackName, moduleAddr)
		}

		if depsRaw, ok := resourceMap["depends_on"].([]interface{}); ok {
			resourceMap["depends_on"] = rewriteDependencies(depsRaw, stackName)
		}

		if instancesRaw, ok := resourceMap["instances"].([]interface{}); ok {
			for idx, inst := range instancesRaw {
				instMap, ok := inst.(map[string]interface{})
				if !ok {
					return 0, fmt.Errorf("resource %d instance %d has unexpected structure: %T", i, idx, inst)
				}

				if depsRaw, ok := instMap["dependencies"].([]interface{}); ok {
					instMap["dependencies"] = rewriteDependencies(depsRaw, stackName)
				}

				if deposedRaw, ok := instMap["deposed"].([]interface{}); ok {
					instMap["deposed"] = rewriteDependencies(deposedRaw, stackName)
				}
			}
		}
	}

	return len(resources), nil
}

func prefixOutputs(state map[string]interface{}, stackName string) int {
	outputsRaw, ok := state["outputs"]
	if !ok {
		return 0
	}

	outputs, ok := outputsRaw.(map[string]interface{})
	if !ok {
		return 0
	}

	newOutputs := make(map[string]interface{}, len(outputs))
	for name, value := range outputs {
		newName := prefixSegment(stackName, name)
		if outputMap, ok := value.(map[string]interface{}); ok {
			if depsRaw, ok := outputMap["depends_on"].([]interface{}); ok {
				outputMap["depends_on"] = rewriteDependencies(depsRaw, stackName)
			}
		}
		newOutputs[newName] = value
	}

	state["outputs"] = newOutputs
	return len(newOutputs)
}

func rewriteDependencies(deps []interface{}, stackName string) []interface{} {
	result := make([]interface{}, 0, len(deps))
	for _, dep := range deps {
		if depStr, ok := dep.(string); ok {
			result = append(result, rewriteAddress(stackName, depStr))
			continue
		}
		result = append(result, dep)
	}
	return result
}

func rewriteAddress(stackName, address string) string {
	if stackName == "" || address == "" {
		return address
	}

	parts := strings.Split(address, ".")
	modulePrefixed := false

	for i := 0; i < len(parts); i++ {
		if parts[i] == "module" && i+1 < len(parts) {
			if !modulePrefixed {
				parts[i+1] = prefixSegment(stackName, parts[i+1])
				modulePrefixed = true
			}
			i++
		}
	}

	if modulePrefixed {
		return strings.Join(parts, ".")
	}

	typeIdx := 0
	if parts[typeIdx] == "data" {
		typeIdx++
	}

	if typeIdx >= len(parts) {
		return strings.Join(parts, ".")
	}

	typeIdx++
	if typeIdx >= len(parts) {
		return strings.Join(parts, ".")
	}

	parts[typeIdx] = prefixSegment(stackName, parts[typeIdx])

	return strings.Join(parts, ".")
}

func rewriteModuleAddress(stackName, address string) string {
	if stackName == "" || address == "" {
		return address
	}

	parts := strings.Split(address, ".")
	modulePrefixed := false

	for i := 0; i < len(parts); i++ {
		if parts[i] == "module" && i+1 < len(parts) {
			if !modulePrefixed {
				parts[i+1] = prefixSegment(stackName, parts[i+1])
				modulePrefixed = true
			}
			i++
		}
	}

	return strings.Join(parts, ".")
}

func prefixSegment(prefix, segment string) string {
	if prefix == "" {
		return segment
	}
	prefixWithUnderscore := prefix + "_"
	if strings.HasPrefix(segment, prefixWithUnderscore) {
		return segment
	}
	return prefixWithUnderscore + segment
}

func sanitizeIdentifier(name string) string {
	if name == "" {
		return ""
	}

	var b strings.Builder
	for _, r := range name {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}

	result := b.String()
	if result == "" {
		return result
	}

	if unicode.IsDigit(rune(result[0])) {
		return "_" + result
	}

	return result
}

func extractResources(state map[string]interface{}) []interface{} {
	raw, ok := state["resources"]
	if !ok {
		return nil
	}

	resources, ok := raw.([]interface{})
	if !ok {
		return nil
	}

	return resources
}

func extractOutputs(state map[string]interface{}) map[string]interface{} {
	raw, ok := state["outputs"]
	if !ok {
		return map[string]interface{}{}
	}

	outputs, ok := raw.(map[string]interface{})
	if !ok {
		return map[string]interface{}{}
	}

	return outputs
}

func extractInt(state map[string]interface{}, key string) int {
	raw, ok := state[key]
	if !ok {
		return 0
	}

	switch v := raw.(type) {
	case float64:
		return int(v)
	case int:
		return v
	default:
		return 0
	}
}

func extractString(state map[string]interface{}, key string) string {
	raw, ok := state[key]
	if !ok {
		return ""
	}
	if s, ok := raw.(string); ok {
		return s
	}
	return ""
}

func collectProviders(state map[string]interface{}, providers map[string]string) {
	resources, ok := state["resources"].([]interface{})
	if !ok {
		return
	}

	for _, res := range resources {
		resMap, ok := res.(map[string]interface{})
		if !ok {
			continue
		}

		if addr, ok := resMap["provider"].(string); ok {
			name, source, valid := parseProviderAddress(addr)
			if !valid {
				continue
			}
			if existing, exists := providers[name]; exists && existing != source {
				fmt.Printf("[!] Warning: provider name %q seen with multiple sources (%s vs %s)\n", name, existing, source)
				continue
			}
			providers[name] = source
		}
	}
}

func parseProviderAddress(addr string) (string, string, bool) {
	if !strings.HasPrefix(addr, "provider[\"") || !strings.HasSuffix(addr, "\"]") {
		return "", "", false
	}

	inner := strings.TrimPrefix(addr, "provider[\"")
	inner = strings.TrimSuffix(inner, "\"]")

	parts := strings.Split(inner, "\",\"")
	if len(parts) == 0 || parts[0] == "" {
		return "", "", false
	}

	source := parts[0]
	segments := strings.Split(source, "/")
	if len(segments) == 0 {
		return "", "", false
	}

	name := segments[len(segments)-1]
	if name == "" {
		return "", "", false
	}

	return name, source, true
}

func stripTagAttributesFromState(state map[string]interface{}) {
	resources, ok := state["resources"].([]interface{})
	if !ok {
		return
	}

	for _, res := range resources {
		resMap, ok := res.(map[string]interface{})
		if !ok {
			continue
		}
		stripTagsFromResourceState(resMap)
	}
}

func stripTagsFromResourceState(resource map[string]interface{}) {
	if resource == nil {
		return
	}

	if attrs, ok := resource["attributes"].(map[string]interface{}); ok {
		removeTagKeys(attrs)
	}

	if values, ok := resource["values"].(map[string]interface{}); ok {
		removeTagKeys(values)
	}

	if instances, ok := resource["instances"].([]interface{}); ok {
		for _, inst := range instances {
			instMap, ok := inst.(map[string]interface{})
			if !ok {
				continue
			}
			stripTagsFromInstanceState(instMap)
		}
	}
}

func stripTagsFromInstanceState(instance map[string]interface{}) {
	if instance == nil {
		return
	}

	if attrs, ok := instance["attributes"].(map[string]interface{}); ok {
		removeTagKeys(attrs)
	}

	if values, ok := instance["values"].(map[string]interface{}); ok {
		removeTagKeys(values)
	}

	if unknown, ok := instance["after_unknown"].(map[string]interface{}); ok {
		removeTagUnknownFlags(unknown)
	}

	if beforeSensitive, ok := instance["before_sensitive"].(map[string]interface{}); ok {
		removeTagKeys(beforeSensitive)
	}

	if afterSensitive, ok := instance["after_sensitive"].(map[string]interface{}); ok {
		removeTagKeys(afterSensitive)
	}

	if nested, ok := instance["deposed"].([]interface{}); ok {
		for _, item := range nested {
			if nestedMap, ok := item.(map[string]interface{}); ok {
				stripTagsFromInstanceState(nestedMap)
			}
		}
	}
}

func removeTagKeys(target map[string]interface{}) {
	if target == nil {
		return
	}

	for _, key := range []string{"tags", "tags_all", "default_tags"} {
		value, ok := target[key]
		if !ok {
			continue
		}

		switch nested := value.(type) {
		case map[string]interface{}:
			removeTagKeys(nested)
		case []interface{}:
			for _, item := range nested {
				if m, ok := item.(map[string]interface{}); ok {
					removeTagKeys(m)
				}
			}
		}

		switch key {
		case "tags", "tags_all":
			if valueMap, ok := value.(map[string]interface{}); ok && len(valueMap) == 0 {
				target[key] = map[string]interface{}{}
			} else {
				target[key] = map[string]interface{}{}
			}
		case "default_tags":
			target[key] = map[string]interface{}{}
		default:
			target[key] = map[string]interface{}{}
		}
	}
}

func removeTagUnknownFlags(target map[string]interface{}) {
	if target == nil {
		return
	}
	for _, key := range []string{"tags", "tags_all", "default_tags"} {
		delete(target, key)
	}
}

func mergeState(resources []interface{}, outputs map[string]interface{}, mergedResources *[]interface{}, mergedOutputs map[string]interface{}) error {
	if resources != nil {
		*mergedResources = append(*mergedResources, resources...)
	}

	for k, v := range outputs {
		if _, exists := mergedOutputs[k]; exists {
			return fmt.Errorf("duplicate output detected: %s", k)
		}
		mergedOutputs[k] = v
	}

	return nil
}

func writeJSON(path string, payload interface{}) error {
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0o644)
}

type renameRule struct {
	search      []string
	replacement []string
}

type renameContext struct {
	rules []renameRule
	seen  map[string]struct{}
}

func newRenameContext() *renameContext {
	return &renameContext{
		seen: make(map[string]struct{}),
	}
}

func (c *renameContext) addRule(search, replacement []string) {
	if len(search) == 0 || len(search) != len(replacement) {
		return
	}
	key := strings.Join(search, "\x00") + "->" + strings.Join(replacement, "\x00")
	if _, exists := c.seen[key]; exists {
		return
	}

	searchCopy := append([]string(nil), search...)
	replacementCopy := append([]string(nil), replacement...)
	c.rules = append(c.rules, renameRule{
		search:      searchCopy,
		replacement: replacementCopy,
	})
	c.seen[key] = struct{}{}
}

type providerRequirement struct {
	Source    string
	HasSource bool

	Constraints map[string]struct{}
	Aliases     map[string]struct{}
	OtherAttrs  map[string]string
}

type providerRequirements map[string]*providerRequirement

// Update this by using terraform providers schema -json and then extracting all of the resources without tags attribute
var tagLifecycleSkipTypes = map[string]struct{}{
	"aws_accessanalyzer_archive_rule":                                  {},
	"aws_account_alternate_contact":                                    {},
	"aws_account_primary_contact":                                      {},
	"aws_account_region":                                               {},
	"aws_acm_certificate_validation":                                   {},
	"aws_acmpca_certificate":                                           {},
	"aws_acmpca_certificate_authority_certificate":                     {},
	"aws_acmpca_permission":                                            {},
	"aws_acmpca_policy":                                                {},
	"aws_alb_listener_certificate":                                     {},
	"aws_alb_target_group_attachment":                                  {},
	"aws_ami_launch_permission":                                        {},
	"aws_amplify_backend_environment":                                  {},
	"aws_amplify_domain_association":                                   {},
	"aws_amplify_webhook":                                              {},
	"aws_api_gateway_account":                                          {},
	"aws_api_gateway_authorizer":                                       {},
	"aws_api_gateway_base_path_mapping":                                {},
	"aws_api_gateway_deployment":                                       {},
	"aws_api_gateway_documentation_part":                               {},
	"aws_api_gateway_documentation_version":                            {},
	"aws_api_gateway_gateway_response":                                 {},
	"aws_api_gateway_integration":                                      {},
	"aws_api_gateway_integration_response":                             {},
	"aws_api_gateway_method":                                           {},
	"aws_api_gateway_method_response":                                  {},
	"aws_api_gateway_method_settings":                                  {},
	"aws_api_gateway_model":                                            {},
	"aws_api_gateway_request_validator":                                {},
	"aws_api_gateway_resource":                                         {},
	"aws_api_gateway_rest_api_policy":                                  {},
	"aws_api_gateway_rest_api_put":                                     {},
	"aws_api_gateway_usage_plan_key":                                   {},
	"aws_apigatewayv2_api_mapping":                                     {},
	"aws_apigatewayv2_authorizer":                                      {},
	"aws_apigatewayv2_deployment":                                      {},
	"aws_apigatewayv2_integration":                                     {},
	"aws_apigatewayv2_integration_response":                            {},
	"aws_apigatewayv2_model":                                           {},
	"aws_apigatewayv2_route":                                           {},
	"aws_apigatewayv2_route_response":                                  {},
	"aws_app_cookie_stickiness_policy":                                 {},
	"aws_appautoscaling_policy":                                        {},
	"aws_appautoscaling_scheduled_action":                              {},
	"aws_appconfig_extension_association":                              {},
	"aws_appconfig_hosted_configuration_version":                       {},
	"aws_appfabric_app_authorization_connection":                       {},
	"aws_appflow_connector_profile":                                    {},
	"aws_apprunner_custom_domain_association":                          {},
	"aws_apprunner_default_auto_scaling_configuration_version":         {},
	"aws_apprunner_deployment":                                         {},
	"aws_appstream_directory_config":                                   {},
	"aws_appstream_fleet_stack_association":                            {},
	"aws_appstream_user":                                               {},
	"aws_appstream_user_stack_association":                             {},
	"aws_appsync_api_cache":                                            {},
	"aws_appsync_api_key":                                              {},
	"aws_appsync_datasource":                                           {},
	"aws_appsync_domain_name":                                          {},
	"aws_appsync_domain_name_api_association":                          {},
	"aws_appsync_function":                                             {},
	"aws_appsync_resolver":                                             {},
	"aws_appsync_source_api_association":                               {},
	"aws_appsync_type":                                                 {},
	"aws_athena_database":                                              {},
	"aws_athena_named_query":                                           {},
	"aws_athena_prepared_statement":                                    {},
	"aws_auditmanager_account_registration":                            {},
	"aws_auditmanager_assessment_delegation":                           {},
	"aws_auditmanager_assessment_report":                               {},
	"aws_auditmanager_framework_share":                                 {},
	"aws_auditmanager_organization_admin_account_registration":         {},
	"aws_autoscaling_attachment":                                       {},
	"aws_autoscaling_group":                                            {},
	"aws_autoscaling_group_tag":                                        {},
	"aws_autoscaling_lifecycle_hook":                                   {},
	"aws_autoscaling_notification":                                     {},
	"aws_autoscaling_policy":                                           {},
	"aws_autoscaling_schedule":                                         {},
	"aws_autoscaling_traffic_source_attachment":                        {},
	"aws_autoscalingplans_scaling_plan":                                {},
	"aws_backup_global_settings":                                       {},
	"aws_backup_region_settings":                                       {},
	"aws_backup_restore_testing_selection":                             {},
	"aws_backup_selection":                                             {},
	"aws_backup_vault_lock_configuration":                              {},
	"aws_backup_vault_notifications":                                   {},
	"aws_backup_vault_policy":                                          {},
	"aws_bedrock_guardrail_version":                                    {},
	"aws_bedrock_model_invocation_logging_configuration":               {},
	"aws_bedrockagent_agent_action_group":                              {},
	"aws_bedrockagent_agent_collaborator":                              {},
	"aws_bedrockagent_agent_knowledge_base_association":                {},
	"aws_bedrockagent_data_source":                                     {},
	"aws_ce_cost_allocation_tag":                                       {},
	"aws_chime_voice_connector_group":                                  {},
	"aws_chime_voice_connector_logging":                                {},
	"aws_chime_voice_connector_origination":                            {},
	"aws_chime_voice_connector_streaming":                              {},
	"aws_chime_voice_connector_termination":                            {},
	"aws_chime_voice_connector_termination_credentials":                {},
	"aws_chimesdkvoice_global_settings":                                {},
	"aws_chimesdkvoice_sip_rule":                                       {},
	"aws_cloud9_environment_membership":                                {},
	"aws_cloudcontrolapi_resource":                                     {},
	"aws_cloudformation_stack_instances":                               {},
	"aws_cloudformation_stack_set_instance":                            {},
	"aws_cloudformation_type":                                          {},
	"aws_cloudfront_cache_policy":                                      {},
	"aws_cloudfront_continuous_deployment_policy":                      {},
	"aws_cloudfront_field_level_encryption_config":                     {},
	"aws_cloudfront_field_level_encryption_profile":                    {},
	"aws_cloudfront_function":                                          {},
	"aws_cloudfront_key_group":                                         {},
	"aws_cloudfront_key_value_store":                                   {},
	"aws_cloudfront_monitoring_subscription":                           {},
	"aws_cloudfront_origin_access_control":                             {},
	"aws_cloudfront_origin_access_identity":                            {},
	"aws_cloudfront_origin_request_policy":                             {},
	"aws_cloudfront_public_key":                                        {},
	"aws_cloudfront_realtime_log_config":                               {},
	"aws_cloudfront_response_headers_policy":                           {},
	"aws_cloudfrontkeyvaluestore_key":                                  {},
	"aws_cloudfrontkeyvaluestore_keys_exclusive":                       {},
	"aws_cloudhsm_v2_hsm":                                              {},
	"aws_cloudsearch_domain":                                           {},
	"aws_cloudsearch_domain_service_access_policy":                     {},
	"aws_cloudtrail_organization_delegated_admin_account":              {},
	"aws_cloudwatch_dashboard":                                         {},
	"aws_cloudwatch_event_api_destination":                             {},
	"aws_cloudwatch_event_archive":                                     {},
	"aws_cloudwatch_event_bus_policy":                                  {},
	"aws_cloudwatch_event_connection":                                  {},
	"aws_cloudwatch_event_endpoint":                                    {},
	"aws_cloudwatch_event_permission":                                  {},
	"aws_cloudwatch_event_target":                                      {},
	"aws_cloudwatch_log_account_policy":                                {},
	"aws_cloudwatch_log_data_protection_policy":                        {},
	"aws_cloudwatch_log_delivery_destination_policy":                   {},
	"aws_cloudwatch_log_destination_policy":                            {},
	"aws_cloudwatch_log_index_policy":                                  {},
	"aws_cloudwatch_log_metric_filter":                                 {},
	"aws_cloudwatch_log_resource_policy":                               {},
	"aws_cloudwatch_log_stream":                                        {},
	"aws_cloudwatch_log_subscription_filter":                           {},
	"aws_cloudwatch_query_definition":                                  {},
	"aws_codeartifact_domain_permissions_policy":                       {},
	"aws_codeartifact_repository_permissions_policy":                   {},
	"aws_codebuild_resource_policy":                                    {},
	"aws_codebuild_source_credential":                                  {},
	"aws_codebuild_webhook":                                            {},
	"aws_codecatalyst_dev_environment":                                 {},
	"aws_codecatalyst_project":                                         {},
	"aws_codecatalyst_source_repository":                               {},
	"aws_codecommit_approval_rule_template":                            {},
	"aws_codecommit_approval_rule_template_association":                {},
	"aws_codecommit_trigger":                                           {},
	"aws_codedeploy_deployment_config":                                 {},
	"aws_codestarconnections_host":                                     {},
	"aws_cognito_identity_pool_provider_principal_tag":                 {},
	"aws_cognito_identity_pool_roles_attachment":                       {},
	"aws_cognito_identity_provider":                                    {},
	"aws_cognito_log_delivery_configuration":                           {},
	"aws_cognito_managed_login_branding":                               {},
	"aws_cognito_managed_user_pool_client":                             {},
	"aws_cognito_resource_server":                                      {},
	"aws_cognito_risk_configuration":                                   {},
	"aws_cognito_user":                                                 {},
	"aws_cognito_user_group":                                           {},
	"aws_cognito_user_in_group":                                        {},
	"aws_cognito_user_pool_client":                                     {},
	"aws_cognito_user_pool_domain":                                     {},
	"aws_cognito_user_pool_ui_customization":                           {},
	"aws_computeoptimizer_enrollment_status":                           {},
	"aws_computeoptimizer_recommendation_preferences":                  {},
	"aws_config_configuration_recorder":                                {},
	"aws_config_configuration_recorder_status":                         {},
	"aws_config_conformance_pack":                                      {},
	"aws_config_delivery_channel":                                      {},
	"aws_config_organization_conformance_pack":                         {},
	"aws_config_organization_custom_policy_rule":                       {},
	"aws_config_organization_custom_rule":                              {},
	"aws_config_organization_managed_rule":                             {},
	"aws_config_remediation_configuration":                             {},
	"aws_config_retention_configuration":                               {},
	"aws_connect_bot_association":                                      {},
	"aws_connect_instance_storage_config":                              {},
	"aws_connect_lambda_function_association":                          {},
	"aws_connect_phone_number_contact_flow_association":                {},
	"aws_connect_user_hierarchy_structure":                             {},
	"aws_controltower_control":                                         {},
	"aws_costoptimizationhub_enrollment_status":                        {},
	"aws_costoptimizationhub_preferences":                              {},
	"aws_customerprofiles_profile":                                     {},
	"aws_dataexchange_event_action":                                    {},
	"aws_datapipeline_pipeline_definition":                             {},
	"aws_datazone_asset_type":                                          {},
	"aws_datazone_environment":                                         {},
	"aws_datazone_environment_blueprint_configuration":                 {},
	"aws_datazone_environment_profile":                                 {},
	"aws_datazone_form_type":                                           {},
	"aws_datazone_glossary":                                            {},
	"aws_datazone_glossary_term":                                       {},
	"aws_datazone_project":                                             {},
	"aws_datazone_user_profile":                                        {},
	"aws_dax_parameter_group":                                          {},
	"aws_dax_subnet_group":                                             {},
	"aws_db_instance_automated_backups_replication":                    {},
	"aws_db_instance_role_association":                                 {},
	"aws_db_proxy_default_target_group":                                {},
	"aws_db_proxy_target":                                              {},
	"aws_detective_invitation_accepter":                                {},
	"aws_detective_member":                                             {},
	"aws_detective_organization_admin_account":                         {},
	"aws_detective_organization_configuration":                         {},
	"aws_devicefarm_upload":                                            {},
	"aws_devopsguru_event_sources_config":                              {},
	"aws_devopsguru_notification_channel":                              {},
	"aws_devopsguru_resource_collection":                               {},
	"aws_devopsguru_service_integration":                               {},
	"aws_directory_service_conditional_forwarder":                      {},
	"aws_directory_service_log_subscription":                           {},
	"aws_directory_service_radius_settings":                            {},
	"aws_directory_service_shared_directory":                           {},
	"aws_directory_service_shared_directory_accepter":                  {},
	"aws_directory_service_trust":                                      {},
	"aws_docdb_cluster_snapshot":                                       {},
	"aws_docdb_global_cluster":                                         {},
	"aws_dsql_cluster_peering":                                         {},
	"aws_dx_bgp_peer":                                                  {},
	"aws_dx_connection_association":                                    {},
	"aws_dx_connection_confirmation":                                   {},
	"aws_dx_gateway":                                                   {},
	"aws_dx_gateway_association":                                       {},
	"aws_dx_gateway_association_proposal":                              {},
	"aws_dx_hosted_connection":                                         {},
	"aws_dx_hosted_private_virtual_interface":                          {},
	"aws_dx_hosted_public_virtual_interface":                           {},
	"aws_dx_hosted_transit_virtual_interface":                          {},
	"aws_dx_macsec_key_association":                                    {},
	"aws_dynamodb_contributor_insights":                                {},
	"aws_dynamodb_global_table":                                        {},
	"aws_dynamodb_kinesis_streaming_destination":                       {},
	"aws_dynamodb_resource_policy":                                     {},
	"aws_dynamodb_table_export":                                        {},
	"aws_dynamodb_table_item":                                          {},
	"aws_dynamodb_tag":                                                 {},
	"aws_ebs_default_kms_key":                                          {},
	"aws_ebs_encryption_by_default":                                    {},
	"aws_ebs_fast_snapshot_restore":                                    {},
	"aws_ebs_snapshot_block_public_access":                             {},
	"aws_ec2_availability_zone_group":                                  {},
	"aws_ec2_client_vpn_authorization_rule":                            {},
	"aws_ec2_client_vpn_network_association":                           {},
	"aws_ec2_client_vpn_route":                                         {},
	"aws_ec2_default_credit_specification":                             {},
	"aws_ec2_image_block_public_access":                                {},
	"aws_ec2_instance_metadata_defaults":                               {},
	"aws_ec2_instance_state":                                           {},
	"aws_ec2_local_gateway_route":                                      {},
	"aws_ec2_managed_prefix_list_entry":                                {},
	"aws_ec2_serial_console_access":                                    {},
	"aws_ec2_subnet_cidr_reservation":                                  {},
	"aws_ec2_tag":                                                      {},
	"aws_ec2_traffic_mirror_filter_rule":                               {},
	"aws_ec2_transit_gateway_default_route_table_association":          {},
	"aws_ec2_transit_gateway_default_route_table_propagation":          {},
	"aws_ec2_transit_gateway_multicast_domain_association":             {},
	"aws_ec2_transit_gateway_multicast_group_member":                   {},
	"aws_ec2_transit_gateway_multicast_group_source":                   {},
	"aws_ec2_transit_gateway_policy_table_association":                 {},
	"aws_ec2_transit_gateway_prefix_list_reference":                    {},
	"aws_ec2_transit_gateway_route":                                    {},
	"aws_ec2_transit_gateway_route_table_association":                  {},
	"aws_ec2_transit_gateway_route_table_propagation":                  {},
	"aws_ecr_account_setting":                                          {},
	"aws_ecr_lifecycle_policy":                                         {},
	"aws_ecr_pull_through_cache_rule":                                  {},
	"aws_ecr_registry_policy":                                          {},
	"aws_ecr_registry_scanning_configuration":                          {},
	"aws_ecr_replication_configuration":                                {},
	"aws_ecr_repository_creation_template":                             {},
	"aws_ecr_repository_policy":                                        {},
	"aws_ecrpublic_repository_policy":                                  {},
	"aws_ecs_account_setting_default":                                  {},
	"aws_ecs_cluster_capacity_providers":                               {},
	"aws_ecs_tag":                                                      {},
	"aws_efs_backup_policy":                                            {},
	"aws_efs_file_system_policy":                                       {},
	"aws_efs_mount_target":                                             {},
	"aws_efs_replication_configuration":                                {},
	"aws_eip_association":                                              {},
	"aws_eip_domain_name":                                              {},
	"aws_eks_access_policy_association":                                {},
	"aws_elastic_beanstalk_configuration_template":                     {},
	"aws_elasticache_global_replication_group":                         {},
	"aws_elasticache_user_group_association":                           {},
	"aws_elasticsearch_domain_policy":                                  {},
	"aws_elasticsearch_domain_saml_options":                            {},
	"aws_elasticsearch_vpc_endpoint":                                   {},
	"aws_elastictranscoder_pipeline":                                   {},
	"aws_elastictranscoder_preset":                                     {},
	"aws_elb_attachment":                                               {},
	"aws_emr_block_public_access_configuration":                        {},
	"aws_emr_instance_fleet":                                           {},
	"aws_emr_instance_group":                                           {},
	"aws_emr_managed_scaling_policy":                                   {},
	"aws_emr_security_configuration":                                   {},
	"aws_emr_studio_session_mapping":                                   {},
	"aws_fms_admin_account":                                            {},
	"aws_fsx_s3_access_point_attachment":                               {},
	"aws_glacier_vault_lock":                                           {},
	"aws_globalaccelerator_custom_routing_endpoint_group":              {},
	"aws_globalaccelerator_custom_routing_listener":                    {},
	"aws_globalaccelerator_endpoint_group":                             {},
	"aws_globalaccelerator_listener":                                   {},
	"aws_glue_catalog_table":                                           {},
	"aws_glue_catalog_table_optimizer":                                 {},
	"aws_glue_classifier":                                              {},
	"aws_glue_data_catalog_encryption_settings":                        {},
	"aws_glue_partition":                                               {},
	"aws_glue_partition_index":                                         {},
	"aws_glue_resource_policy":                                         {},
	"aws_glue_security_configuration":                                  {},
	"aws_glue_user_defined_function":                                   {},
	"aws_grafana_license_association":                                  {},
	"aws_grafana_role_association":                                     {},
	"aws_grafana_workspace_api_key":                                    {},
	"aws_grafana_workspace_saml_configuration":                         {},
	"aws_grafana_workspace_service_account":                            {},
	"aws_grafana_workspace_service_account_token":                      {},
	"aws_guardduty_detector_feature":                                   {},
	"aws_guardduty_invite_accepter":                                    {},
	"aws_guardduty_member":                                             {},
	"aws_guardduty_member_detector_feature":                            {},
	"aws_guardduty_organization_admin_account":                         {},
	"aws_guardduty_organization_configuration":                         {},
	"aws_guardduty_organization_configuration_feature":                 {},
	"aws_guardduty_publishing_destination":                             {},
	"aws_iam_access_key":                                               {},
	"aws_iam_account_alias":                                            {},
	"aws_iam_account_password_policy":                                  {},
	"aws_iam_group":                                                    {},
	"aws_iam_group_membership":                                         {},
	"aws_iam_group_policies_exclusive":                                 {},
	"aws_iam_group_policy":                                             {},
	"aws_iam_group_policy_attachment":                                  {},
	"aws_iam_group_policy_attachments_exclusive":                       {},
	"aws_iam_organizations_features":                                   {},
	"aws_iam_policy_attachment":                                        {},
	"aws_iam_role_policies_exclusive":                                  {},
	"aws_iam_role_policy":                                              {},
	"aws_iam_role_policy_attachment":                                   {},
	"aws_iam_role_policy_attachments_exclusive":                        {},
	"aws_iam_security_token_service_preferences":                       {},
	"aws_iam_service_specific_credential":                              {},
	"aws_iam_signing_certificate":                                      {},
	"aws_iam_user_group_membership":                                    {},
	"aws_iam_user_login_profile":                                       {},
	"aws_iam_user_policies_exclusive":                                  {},
	"aws_iam_user_policy":                                              {},
	"aws_iam_user_policy_attachment":                                   {},
	"aws_iam_user_policy_attachments_exclusive":                        {},
	"aws_iam_user_ssh_key":                                             {},
	"aws_identitystore_group":                                          {},
	"aws_identitystore_group_membership":                               {},
	"aws_identitystore_user":                                           {},
	"aws_inspector2_delegated_admin_account":                           {},
	"aws_inspector2_enabler":                                           {},
	"aws_inspector2_member_association":                                {},
	"aws_inspector2_organization_configuration":                        {},
	"aws_inspector_assessment_target":                                  {},
	"aws_internet_gateway_attachment":                                  {},
	"aws_iot_certificate":                                              {},
	"aws_iot_event_configurations":                                     {},
	"aws_iot_indexing_configuration":                                   {},
	"aws_iot_logging_options":                                          {},
	"aws_iot_policy_attachment":                                        {},
	"aws_iot_thing":                                                    {},
	"aws_iot_thing_group_membership":                                   {},
	"aws_iot_thing_principal_attachment":                               {},
	"aws_iot_topic_rule_destination":                                   {},
	"aws_kendra_experience":                                            {},
	"aws_kinesis_resource_policy":                                      {},
	"aws_kinesisanalyticsv2_application_snapshot":                      {},
	"aws_kms_alias":                                                    {},
	"aws_kms_ciphertext":                                               {},
	"aws_kms_custom_key_store":                                         {},
	"aws_kms_grant":                                                    {},
	"aws_kms_key_policy":                                               {},
	"aws_lakeformation_data_cells_filter":                              {},
	"aws_lakeformation_data_lake_settings":                             {},
	"aws_lakeformation_lf_tag":                                         {},
	"aws_lakeformation_lf_tag_expression":                              {},
	"aws_lakeformation_opt_in":                                         {},
	"aws_lakeformation_permissions":                                    {},
	"aws_lakeformation_resource":                                       {},
	"aws_lakeformation_resource_lf_tag":                                {},
	"aws_lakeformation_resource_lf_tags":                               {},
	"aws_lambda_alias":                                                 {},
	"aws_lambda_function_event_invoke_config":                          {},
	"aws_lambda_function_recursion_config":                             {},
	"aws_lambda_function_url":                                          {},
	"aws_lambda_invocation":                                            {},
	"aws_lambda_layer_version":                                         {},
	"aws_lambda_layer_version_permission":                              {},
	"aws_lambda_permission":                                            {},
	"aws_lambda_provisioned_concurrency_config":                        {},
	"aws_lambda_runtime_management_config":                             {},
	"aws_launch_configuration":                                         {},
	"aws_lb_cookie_stickiness_policy":                                  {},
	"aws_lb_listener_certificate":                                      {},
	"aws_lb_ssl_negotiation_policy":                                    {},
	"aws_lb_target_group_attachment":                                   {},
	"aws_lb_trust_store_revocation":                                    {},
	"aws_lex_bot":                                                      {},
	"aws_lex_bot_alias":                                                {},
	"aws_lex_intent":                                                   {},
	"aws_lex_slot_type":                                                {},
	"aws_lexv2models_bot_locale":                                       {},
	"aws_lexv2models_bot_version":                                      {},
	"aws_lexv2models_intent":                                           {},
	"aws_lexv2models_slot":                                             {},
	"aws_lexv2models_slot_type":                                        {},
	"aws_licensemanager_association":                                   {},
	"aws_licensemanager_grant":                                         {},
	"aws_licensemanager_grant_accepter":                                {},
	"aws_lightsail_bucket_access_key":                                  {},
	"aws_lightsail_bucket_resource_access":                             {},
	"aws_lightsail_container_service_deployment_version":               {},
	"aws_lightsail_disk_attachment":                                    {},
	"aws_lightsail_domain":                                             {},
	"aws_lightsail_domain_entry":                                       {},
	"aws_lightsail_instance_public_ports":                              {},
	"aws_lightsail_lb_attachment":                                      {},
	"aws_lightsail_lb_certificate":                                     {},
	"aws_lightsail_lb_certificate_attachment":                          {},
	"aws_lightsail_lb_https_redirection_policy":                        {},
	"aws_lightsail_lb_stickiness_policy":                               {},
	"aws_lightsail_static_ip":                                          {},
	"aws_lightsail_static_ip_attachment":                               {},
	"aws_load_balancer_backend_server_policy":                          {},
	"aws_load_balancer_listener_policy":                                {},
	"aws_load_balancer_policy":                                         {},
	"aws_location_tracker_association":                                 {},
	"aws_m2_deployment":                                                {},
	"aws_macie2_account":                                               {},
	"aws_macie2_classification_export_configuration":                   {},
	"aws_macie2_invitation_accepter":                                   {},
	"aws_macie2_organization_admin_account":                            {},
	"aws_macie2_organization_configuration":                            {},
	"aws_main_route_table_association":                                 {},
	"aws_media_store_container_policy":                                 {},
	"aws_medialive_multiplex_program":                                  {},
	"aws_msk_cluster_policy":                                           {},
	"aws_msk_configuration":                                            {},
	"aws_msk_scram_secret_association":                                 {},
	"aws_msk_single_scram_secret_association":                          {},
	"aws_nat_gateway_eip_association":                                  {},
	"aws_neptune_cluster_snapshot":                                     {},
	"aws_neptune_global_cluster":                                       {},
	"aws_network_acl_association":                                      {},
	"aws_network_acl_rule":                                             {},
	"aws_network_interface_attachment":                                 {},
	"aws_network_interface_permission":                                 {},
	"aws_network_interface_sg_attachment":                              {},
	"aws_networkfirewall_firewall_transit_gateway_attachment_accepter": {},
	"aws_networkfirewall_logging_configuration":                        {},
	"aws_networkfirewall_resource_policy":                              {},
	"aws_networkmanager_attachment_accepter":                           {},
	"aws_networkmanager_core_network_policy_attachment":                {},
	"aws_networkmanager_customer_gateway_association":                  {},
	"aws_networkmanager_link_association":                              {},
	"aws_networkmanager_transit_gateway_connect_peer_association":      {},
	"aws_networkmanager_transit_gateway_registration":                  {},
	"aws_notifications_channel_association":                            {},
	"aws_notifications_event_rule":                                     {},
	"aws_notifications_notification_hub":                               {},
	"aws_oam_sink_policy":                                              {},
	"aws_opensearch_authorize_vpc_endpoint_access":                     {},
	"aws_opensearch_domain_policy":                                     {},
	"aws_opensearch_domain_saml_options":                               {},
	"aws_opensearch_inbound_connection_accepter":                       {},
	"aws_opensearch_outbound_connection":                               {},
	"aws_opensearch_package":                                           {},
	"aws_opensearch_package_association":                               {},
	"aws_opensearch_vpc_endpoint":                                      {},
	"aws_opensearchserverless_access_policy":                           {},
	"aws_opensearchserverless_lifecycle_policy":                        {},
	"aws_opensearchserverless_security_config":                         {},
	"aws_opensearchserverless_security_policy":                         {},
	"aws_opensearchserverless_vpc_endpoint":                            {},
	"aws_organizations_delegated_administrator":                        {},
	"aws_organizations_organization":                                   {},
	"aws_organizations_policy_attachment":                              {},
	"aws_paymentcryptography_key_alias":                                {},
	"aws_pinpoint_adm_channel":                                         {},
	"aws_pinpoint_apns_channel":                                        {},
	"aws_pinpoint_apns_sandbox_channel":                                {},
	"aws_pinpoint_apns_voip_channel":                                   {},
	"aws_pinpoint_apns_voip_sandbox_channel":                           {},
	"aws_pinpoint_baidu_channel":                                       {},
	"aws_pinpoint_email_channel":                                       {},
	"aws_pinpoint_event_stream":                                        {},
	"aws_pinpoint_gcm_channel":                                         {},
	"aws_pinpoint_sms_channel":                                         {},
	"aws_prometheus_alert_manager_definition":                          {},
	"aws_prometheus_query_logging_configuration":                       {},
	"aws_prometheus_resource_policy":                                   {},
	"aws_prometheus_workspace_configuration":                           {},
	"aws_proxy_protocol_policy":                                        {},
	"aws_quicksight_account_settings":                                  {},
	"aws_quicksight_account_subscription":                              {},
	"aws_quicksight_folder_membership":                                 {},
	"aws_quicksight_group":                                             {},
	"aws_quicksight_group_membership":                                  {},
	"aws_quicksight_iam_policy_assignment":                             {},
	"aws_quicksight_ingestion":                                         {},
	"aws_quicksight_ip_restriction":                                    {},
	"aws_quicksight_key_registration":                                  {},
	"aws_quicksight_refresh_schedule":                                  {},
	"aws_quicksight_role_custom_permission":                            {},
	"aws_quicksight_role_membership":                                   {},
	"aws_quicksight_template_alias":                                    {},
	"aws_quicksight_user":                                              {},
	"aws_quicksight_user_custom_permission":                            {},
	"aws_ram_principal_association":                                    {},
	"aws_ram_resource_association":                                     {},
	"aws_ram_resource_share_accepter":                                  {},
	"aws_ram_sharing_with_organization":                                {},
	"aws_rds_certificate":                                              {},
	"aws_rds_cluster_activity_stream":                                  {},
	"aws_rds_cluster_role_association":                                 {},
	"aws_rds_export_task":                                              {},
	"aws_rds_instance_state":                                           {},
	"aws_redshift_authentication_profile":                              {},
	"aws_redshift_cluster_iam_roles":                                   {},
	"aws_redshift_data_share_authorization":                            {},
	"aws_redshift_data_share_consumer_association":                     {},
	"aws_redshift_endpoint_access":                                     {},
	"aws_redshift_endpoint_authorization":                              {},
	"aws_redshift_logging":                                             {},
	"aws_redshift_partner":                                             {},
	"aws_redshift_resource_policy":                                     {},
	"aws_redshift_scheduled_action":                                    {},
	"aws_redshift_snapshot_copy":                                       {},
	"aws_redshift_snapshot_schedule_association":                       {},
	"aws_redshiftdata_statement":                                       {},
	"aws_redshiftserverless_custom_domain_association":                 {},
	"aws_redshiftserverless_endpoint_access":                           {},
	"aws_redshiftserverless_resource_policy":                           {},
	"aws_redshiftserverless_snapshot":                                  {},
	"aws_redshiftserverless_usage_limit":                               {},
	"aws_resourcegroups_resource":                                      {},
	"aws_route":                                                        {},
	"aws_route53_cidr_collection":                                      {},
	"aws_route53_cidr_location":                                        {},
	"aws_route53_delegation_set":                                       {},
	"aws_route53_hosted_zone_dnssec":                                   {},
	"aws_route53_key_signing_key":                                      {},
	"aws_route53_query_log":                                            {},
	"aws_route53_record":                                               {},
	"aws_route53_records_exclusive":                                    {},
	"aws_route53_resolver_config":                                      {},
	"aws_route53_resolver_dnssec_config":                               {},
	"aws_route53_resolver_firewall_config":                             {},
	"aws_route53_resolver_firewall_rule":                               {},
	"aws_route53_resolver_query_log_config_association":                {},
	"aws_route53_resolver_rule_association":                            {},
	"aws_route53_traffic_policy":                                       {},
	"aws_route53_traffic_policy_instance":                              {},
	"aws_route53_vpc_association_authorization":                        {},
	"aws_route53_zone_association":                                     {},
	"aws_route53domains_delegation_signer_record":                      {},
	"aws_route53profiles_resource_association":                         {},
	"aws_route53recoverycontrolconfig_routing_control":                 {},
	"aws_route_table_association":                                      {},
	"aws_rum_metrics_destination":                                      {},
	"aws_s3_account_public_access_block":                               {},
	"aws_s3_bucket_accelerate_configuration":                           {},
	"aws_s3_bucket_acl":                                                {},
	"aws_s3_bucket_analytics_configuration":                            {},
	"aws_s3_bucket_cors_configuration":                                 {},
	"aws_s3_bucket_intelligent_tiering_configuration":                  {},
	"aws_s3_bucket_inventory":                                          {},
	"aws_s3_bucket_lifecycle_configuration":                            {},
	"aws_s3_bucket_logging":                                            {},
	"aws_s3_bucket_metadata_configuration":                             {},
	"aws_s3_bucket_metric":                                             {},
	"aws_s3_bucket_notification":                                       {},
	"aws_s3_bucket_object_lock_configuration":                          {},
	"aws_s3_bucket_ownership_controls":                                 {},
	"aws_s3_bucket_policy":                                             {},
	"aws_s3_bucket_public_access_block":                                {},
	"aws_s3_bucket_replication_configuration":                          {},
	"aws_s3_bucket_request_payment_configuration":                      {},
	"aws_s3_bucket_server_side_encryption_configuration":               {},
	"aws_s3_bucket_versioning":                                         {},
	"aws_s3_bucket_website_configuration":                              {},
	"aws_s3control_access_grants_instance_resource_policy":             {},
	"aws_s3control_access_point_policy":                                {},
	"aws_s3control_bucket_lifecycle_configuration":                     {},
	"aws_s3control_bucket_policy":                                      {},
	"aws_s3control_directory_bucket_access_point_scope":                {},
	"aws_s3control_multi_region_access_point":                          {},
	"aws_s3control_multi_region_access_point_policy":                   {},
	"aws_s3control_object_lambda_access_point":                         {},
	"aws_s3control_object_lambda_access_point_policy":                  {},
	"aws_s3outposts_endpoint":                                          {},
	"aws_s3tables_namespace":                                           {},
	"aws_s3tables_table":                                               {},
	"aws_s3tables_table_bucket":                                        {},
	"aws_s3tables_table_bucket_policy":                                 {},
	"aws_s3tables_table_policy":                                        {},
	"aws_sagemaker_device":                                             {},
	"aws_sagemaker_image_version":                                      {},
	"aws_sagemaker_model_package_group_policy":                         {},
	"aws_sagemaker_servicecatalog_portfolio_status":                    {},
	"aws_sagemaker_workforce":                                          {},
	"aws_scheduler_schedule":                                           {},
	"aws_schemas_registry_policy":                                      {},
	"aws_secretsmanager_secret_policy":                                 {},
	"aws_secretsmanager_secret_rotation":                               {},
	"aws_secretsmanager_secret_version":                                {},
	"aws_security_group_rule":                                          {},
	"aws_securityhub_account":                                          {},
	"aws_securityhub_action_target":                                    {},
	"aws_securityhub_configuration_policy":                             {},
	"aws_securityhub_configuration_policy_association":                 {},
	"aws_securityhub_finding_aggregator":                               {},
	"aws_securityhub_insight":                                          {},
	"aws_securityhub_invite_accepter":                                  {},
	"aws_securityhub_member":                                           {},
	"aws_securityhub_organization_admin_account":                       {},
	"aws_securityhub_organization_configuration":                       {},
	"aws_securityhub_product_subscription":                             {},
	"aws_securityhub_standards_control":                                {},
	"aws_securityhub_standards_control_association":                    {},
	"aws_securityhub_standards_subscription":                           {},
	"aws_securitylake_aws_log_source":                                  {},
	"aws_securitylake_custom_log_source":                               {},
	"aws_securitylake_subscriber_notification":                         {},
	"aws_service_discovery_instance":                                   {},
	"aws_servicecatalog_budget_resource_association":                   {},
	"aws_servicecatalog_constraint":                                    {},
	"aws_servicecatalog_organizations_access":                          {},
	"aws_servicecatalog_portfolio_share":                               {},
	"aws_servicecatalog_principal_portfolio_association":               {},
	"aws_servicecatalog_product_portfolio_association":                 {},
	"aws_servicecatalog_provisioning_artifact":                         {},
	"aws_servicecatalog_service_action":                                {},
	"aws_servicecatalog_tag_option":                                    {},
	"aws_servicecatalog_tag_option_resource_association":               {},
	"aws_servicecatalogappregistry_attribute_group_association":        {},
	"aws_servicequotas_service_quota":                                  {},
	"aws_servicequotas_template":                                       {},
	"aws_servicequotas_template_association":                           {},
	"aws_ses_active_receipt_rule_set":                                  {},
	"aws_ses_configuration_set":                                        {},
	"aws_ses_domain_dkim":                                              {},
	"aws_ses_domain_identity":                                          {},
	"aws_ses_domain_identity_verification":                             {},
	"aws_ses_domain_mail_from":                                         {},
	"aws_ses_email_identity":                                           {},
	"aws_ses_event_destination":                                        {},
	"aws_ses_identity_notification_topic":                              {},
	"aws_ses_identity_policy":                                          {},
	"aws_ses_receipt_filter":                                           {},
	"aws_ses_receipt_rule":                                             {},
	"aws_ses_receipt_rule_set":                                         {},
	"aws_ses_template":                                                 {},
	"aws_sesv2_account_suppression_attributes":                         {},
	"aws_sesv2_account_vdm_attributes":                                 {},
	"aws_sesv2_configuration_set_event_destination":                    {},
	"aws_sesv2_dedicated_ip_assignment":                                {},
	"aws_sesv2_email_identity_feedback_attributes":                     {},
	"aws_sesv2_email_identity_mail_from_attributes":                    {},
	"aws_sesv2_email_identity_policy":                                  {},
	"aws_sfn_alias":                                                    {},
	"aws_shield_application_layer_automatic_response":                  {},
	"aws_shield_drt_access_log_bucket_association":                     {},
	"aws_shield_drt_access_role_arn_association":                       {},
	"aws_shield_proactive_engagement":                                  {},
	"aws_shield_protection_health_check_association":                   {},
	"aws_shield_subscription":                                          {},
	"aws_signer_signing_job":                                           {},
	"aws_signer_signing_profile_permission":                            {},
	"aws_snapshot_create_volume_permission":                            {},
	"aws_sns_platform_application":                                     {},
	"aws_sns_sms_preferences":                                          {},
	"aws_sns_topic_data_protection_policy":                             {},
	"aws_sns_topic_policy":                                             {},
	"aws_sns_topic_subscription":                                       {},
	"aws_spot_datafeed_subscription":                                   {},
	"aws_sqs_queue_policy":                                             {},
	"aws_sqs_queue_redrive_allow_policy":                               {},
	"aws_sqs_queue_redrive_policy":                                     {},
	"aws_ssm_default_patch_baseline":                                   {},
	"aws_ssm_maintenance_window_target":                                {},
	"aws_ssm_maintenance_window_task":                                  {},
	"aws_ssm_patch_group":                                              {},
	"aws_ssm_resource_data_sync":                                       {},
	"aws_ssm_service_setting":                                          {},
	"aws_ssmcontacts_contact_channel":                                  {},
	"aws_ssmcontacts_plan":                                             {},
	"aws_ssoadmin_account_assignment":                                  {},
	"aws_ssoadmin_application_access_scope":                            {},
	"aws_ssoadmin_application_assignment":                              {},
	"aws_ssoadmin_application_assignment_configuration":                {},
	"aws_ssoadmin_customer_managed_policy_attachment":                  {},
	"aws_ssoadmin_instance_access_control_attributes":                  {},
	"aws_ssoadmin_managed_policy_attachment":                           {},
	"aws_ssoadmin_permission_set_inline_policy":                        {},
	"aws_ssoadmin_permissions_boundary_attachment":                     {},
	"aws_storagegateway_cache":                                         {},
	"aws_storagegateway_upload_buffer":                                 {},
	"aws_storagegateway_working_storage":                               {},
	"aws_synthetics_group_association":                                 {},
	"aws_transfer_access":                                              {},
	"aws_transfer_ssh_key":                                             {},
	"aws_transfer_tag":                                                 {},
	"aws_transfer_web_app_customization":                               {},
	"aws_verifiedaccess_instance_logging_configuration":                {},
	"aws_verifiedaccess_instance_trust_provider_attachment":            {},
	"aws_verifiedpermissions_identity_source":                          {},
	"aws_verifiedpermissions_policy":                                   {},
	"aws_verifiedpermissions_policy_template":                          {},
	"aws_verifiedpermissions_schema":                                   {},
	"aws_volume_attachment":                                            {},
	"aws_vpc_block_public_access_options":                              {},
	"aws_vpc_dhcp_options_association":                                 {},
	"aws_vpc_endpoint_connection_accepter":                             {},
	"aws_vpc_endpoint_connection_notification":                         {},
	"aws_vpc_endpoint_policy":                                          {},
	"aws_vpc_endpoint_private_dns":                                     {},
	"aws_vpc_endpoint_route_table_association":                         {},
	"aws_vpc_endpoint_security_group_association":                      {},
	"aws_vpc_endpoint_service_allowed_principal":                       {},
	"aws_vpc_endpoint_service_private_dns_verification":                {},
	"aws_vpc_endpoint_subnet_association":                              {},
	"aws_vpc_ipam_organization_admin_account":                          {},
	"aws_vpc_ipam_pool_cidr":                                           {},
	"aws_vpc_ipam_pool_cidr_allocation":                                {},
	"aws_vpc_ipam_preview_next_cidr":                                   {},
	"aws_vpc_ipv4_cidr_block_association":                              {},
	"aws_vpc_ipv6_cidr_block_association":                              {},
	"aws_vpc_network_performance_metric_subscription":                  {},
	"aws_vpc_peering_connection_options":                               {},
	"aws_vpc_route_server_propagation":                                 {},
	"aws_vpc_route_server_vpc_association":                             {},
	"aws_vpc_security_group_vpc_association":                           {},
	"aws_vpclattice_auth_policy":                                       {},
	"aws_vpclattice_resource_policy":                                   {},
	"aws_vpclattice_target_group_attachment":                           {},
	"aws_vpn_connection_route":                                         {},
	"aws_vpn_gateway_attachment":                                       {},
	"aws_vpn_gateway_route_propagation":                                {},
	"aws_waf_byte_match_set":                                           {},
	"aws_waf_geo_match_set":                                            {},
	"aws_waf_ipset":                                                    {},
	"aws_waf_regex_match_set":                                          {},
	"aws_waf_regex_pattern_set":                                        {},
	"aws_waf_size_constraint_set":                                      {},
	"aws_waf_sql_injection_match_set":                                  {},
	"aws_waf_xss_match_set":                                            {},
	"aws_wafregional_byte_match_set":                                   {},
	"aws_wafregional_geo_match_set":                                    {},
	"aws_wafregional_ipset":                                            {},
	"aws_wafregional_regex_match_set":                                  {},
	"aws_wafregional_regex_pattern_set":                                {},
	"aws_wafregional_size_constraint_set":                              {},
	"aws_wafregional_sql_injection_match_set":                          {},
	"aws_wafregional_web_acl_association":                              {},
	"aws_wafregional_xss_match_set":                                    {},
	"aws_wafv2_api_key":                                                {},
	"aws_wafv2_web_acl_association":                                    {},
	"aws_wafv2_web_acl_logging_configuration":                          {},
	"aws_wafv2_web_acl_rule_group_association":                         {},
	"aws_workspacesweb_browser_settings_association":                   {},
	"aws_workspacesweb_data_protection_settings_association":           {},
	"aws_workspacesweb_ip_access_settings_association":                 {},
	"aws_workspacesweb_network_settings_association":                   {},
	"aws_workspacesweb_session_logger_association":                     {},
	"aws_workspacesweb_trust_store_association":                        {},
	"aws_workspacesweb_user_access_logging_settings_association":       {},
	"aws_workspacesweb_user_settings_association":                      {},
	"aws_xray_encryption_config":                                       {},
	"aws_xray_resource_policy":                                         {},
}

func newProviderRequirement() *providerRequirement {
	return &providerRequirement{
		Constraints: make(map[string]struct{}),
		Aliases:     make(map[string]struct{}),
		OtherAttrs:  make(map[string]string),
	}
}

func (pr providerRequirements) merge(name string, tokens hclwrite.Tokens) error {
	if pr == nil {
		return fmt.Errorf("provider requirements map not initialised")
	}

	req, err := parseProviderRequirement(tokens)
	if err != nil {
		return err
	}

	if existing, ok := pr[name]; ok {
		existing.merge(name, req)
		return nil
	}

	pr[name] = req
	return nil
}

func (pr *providerRequirement) merge(name string, incoming *providerRequirement) {
	if pr == nil || incoming == nil {
		return
	}

	if incoming.HasSource {
		pr.mergeSource(name, incoming.Source)
	}

	for constraint := range incoming.Constraints {
		pr.Constraints[constraint] = struct{}{}
	}

	for alias := range incoming.Aliases {
		pr.Aliases[alias] = struct{}{}
	}

	for key, expr := range incoming.OtherAttrs {
		if existing, ok := pr.OtherAttrs[key]; ok {
			if existing != strings.TrimSpace(expr) {
				fmt.Printf("[!] Warning: conflicting %s for provider %q; keeping first definition\n", key, name)
			}
			continue
		}
		pr.OtherAttrs[key] = strings.TrimSpace(expr)
	}
}

func (pr *providerRequirement) mergeSource(name, incoming string) {
	if !pr.HasSource || pr.Source == "" {
		pr.Source = incoming
		pr.HasSource = incoming != ""
		return
	}
	if incoming == "" || incoming == pr.Source {
		return
	}

	incomingPreferred := isHashicorpSource(incoming) && !isHashicorpSource(pr.Source)
	if incomingPreferred {
		fmt.Printf("[!] Warning: conflicting source for provider %q; preferring %q over %q\n", name, incoming, pr.Source)
		pr.Source = incoming
		return
	}

	if pr.Source != incoming {
		fmt.Printf("[!] Warning: conflicting source for provider %q (%q vs %q); keeping %q\n", name, pr.Source, incoming, pr.Source)
	}
}

func isHashicorpSource(src string) bool {
	return strings.Contains(src, "hashicorp/")
}

func (pr *providerRequirement) versionString() string {
	if len(pr.Constraints) == 0 {
		return ""
	}
	constraints := make([]string, 0, len(pr.Constraints))
	for c := range pr.Constraints {
		constraints = append(constraints, c)
	}
	sort.Strings(constraints)
	return strings.Join(constraints, ", ")
}

func (pr *providerRequirement) aliasesList() []string {
	if len(pr.Aliases) == 0 {
		return nil
	}
	aliases := make([]string, 0, len(pr.Aliases))
	for alias := range pr.Aliases {
		aliases = append(aliases, alias)
	}
	sort.Strings(aliases)
	return aliases
}

func (pr *providerRequirement) renderAliases() string {
	aliases := pr.aliasesList()
	if len(aliases) == 0 {
		return ""
	}
	return "[" + strings.Join(aliases, ", ") + "]"
}

func (pr *providerRequirement) tokens() (hclwrite.Tokens, error) {
	var builder strings.Builder
	builder.WriteString("{\n")

	if pr.HasSource && pr.Source != "" {
		builder.WriteString(formatAttribute("source", fmt.Sprintf("%q", pr.Source)))
	}

	if version := pr.versionString(); version != "" {
		builder.WriteString(formatAttribute("version", fmt.Sprintf("%q", version)))
	}

	if aliasesExpr := pr.renderAliases(); aliasesExpr != "" {
		builder.WriteString(formatAttribute("configuration_aliases", aliasesExpr))
	}

	if len(pr.OtherAttrs) > 0 {
		names := make([]string, 0, len(pr.OtherAttrs))
		for name := range pr.OtherAttrs {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			expr := pr.OtherAttrs[name]
			if expr == "" {
				continue
			}
			builder.WriteString(formatAttribute(name, expr))
		}
	}

	builder.WriteString("}")

	tokens, err := tokensForExpression(builder.String())
	if err != nil {
		return nil, fmt.Errorf("render provider requirement: %w", err)
	}
	return tokens, nil
}

func formatAttribute(name, expr string) string {
	expr = strings.TrimSpace(expr)
	if expr == "" {
		return ""
	}

	lines := strings.Split(expr, "\n")
	var builder strings.Builder
	builder.WriteString("  ")
	builder.WriteString(name)
	builder.WriteString(" = ")
	builder.WriteString(lines[0])
	builder.WriteString("\n")

	for _, line := range lines[1:] {
		builder.WriteString("    ")
		builder.WriteString(line)
		builder.WriteString("\n")
	}

	return builder.String()
}

func tokensForExpression(expr string) (hclwrite.Tokens, error) {
	src := fmt.Sprintf("value = %s", expr)
	file, diags := hclwrite.ParseConfig([]byte(src), "generated.hcl", hcl.InitialPos)
	if diags.HasErrors() {
		return nil, fmt.Errorf("parse expression: %s", diags.Error())
	}

	attr := file.Body().GetAttribute("value")
	if attr == nil {
		return nil, fmt.Errorf("generated expression missing attribute")
	}

	return copyTokens(attr.Expr().BuildTokens(nil)), nil
}

func parseProviderRequirement(tokens hclwrite.Tokens) (*providerRequirement, error) {
	expr := strings.TrimSpace(tokensToString(tokens))
	if expr == "" {
		return newProviderRequirement(), nil
	}

	src := fmt.Sprintf("value = %s", expr)
	file, diags := hclsyntax.ParseConfig([]byte(src), "provider.hcl", hcl.InitialPos)
	if diags.HasErrors() {
		return nil, fmt.Errorf("parse required_providers entry: %s", diags.Error())
	}

	body, ok := file.Body.(*hclsyntax.Body)
	if !ok {
		return nil, fmt.Errorf("unexpected body type for provider requirement")
	}

	attr, exists := body.Attributes["value"]
	if !exists {
		return nil, fmt.Errorf("missing value attribute in parsed provider requirement")
	}

	obj, ok := attr.Expr.(*hclsyntax.ObjectConsExpr)
	if !ok {
		return nil, fmt.Errorf("expected object expression for provider requirement")
	}

	req := newProviderRequirement()
	srcBytes := []byte(src)

	for _, item := range obj.Items {
		key, err := objectKeyString(item.KeyExpr, srcBytes)
		if err != nil {
			return nil, fmt.Errorf("invalid key in provider requirement: %w", err)
		}

		switch key {
		case "source":
			value, err := expressionString(item.ValueExpr)
			if err != nil {
				return nil, fmt.Errorf("invalid source for provider requirement: %w", err)
			}
			req.Source = value
			req.HasSource = value != ""
		case "version":
			value, err := expressionString(item.ValueExpr)
			if err != nil {
				return nil, fmt.Errorf("invalid version for provider requirement: %w", err)
			}
			for _, constraint := range splitConstraints(value) {
				req.Constraints[constraint] = struct{}{}
			}
		case "configuration_aliases":
			aliases, err := parseAliasExpressions(item.ValueExpr, srcBytes)
			if err != nil {
				return nil, fmt.Errorf("invalid configuration_aliases for provider requirement: %w", err)
			}
			for _, alias := range aliases {
				req.Aliases[alias] = struct{}{}
			}
		default:
			req.OtherAttrs[key] = strings.TrimSpace(extractExpression(item.ValueExpr, srcBytes))
		}
	}

	return req, nil
}

func tokensToString(tokens hclwrite.Tokens) string {
	var builder strings.Builder
	for _, tok := range tokens {
		if tok == nil {
			continue
		}
		if tok.SpacesBefore > 0 {
			builder.WriteString(strings.Repeat(" ", tok.SpacesBefore))
		}
		builder.Write(tok.Bytes)
	}
	return builder.String()
}

func objectKeyString(expr hclsyntax.Expression, src []byte) (string, error) {
	switch keyExpr := expr.(type) {
	case *hclsyntax.ObjectConsKeyExpr:
		value, diags := keyExpr.Value(nil)
		if diags.HasErrors() {
			return "", fmt.Errorf("%s", diags.Error())
		}
		if value.Type() != cty.String {
			return "", fmt.Errorf("expected string key, got %s", value.GoString())
		}
		return value.AsString(), nil
	default:
		value, diags := expr.Value(nil)
		if diags.HasErrors() {
			return "", fmt.Errorf("%s", diags.Error())
		}
		if value.Type() != cty.String {
			return "", fmt.Errorf("expected string key")
		}
		return value.AsString(), nil
	}
}

func expressionString(expr hclsyntax.Expression) (string, error) {
	value, diags := expr.Value(nil)
	if diags.HasErrors() {
		return "", fmt.Errorf("%s", diags.Error())
	}
	if value.Type() != cty.String {
		return "", fmt.Errorf("expected string literal")
	}
	return value.AsString(), nil
}

func extractExpression(expr hclsyntax.Expression, src []byte) string {
	rng := expr.Range()
	if rng.End.Byte > len(src) || rng.End.Byte <= rng.Start.Byte {
		return ""
	}

	return string(src[rng.Start.Byte:rng.End.Byte])
}

func parseAliasExpressions(expr hclsyntax.Expression, src []byte) ([]string, error) {
	tuple, ok := expr.(*hclsyntax.TupleConsExpr)
	if !ok {
		return nil, fmt.Errorf("expected list expression for configuration_aliases")
	}

	var aliases []string
	for _, element := range tuple.Exprs {
		alias := strings.TrimSpace(extractExpression(element, src))
		if alias == "" {
			continue
		}
		aliases = append(aliases, alias)
	}
	return aliases, nil
}

func splitConstraints(raw string) []string {
	chunks := strings.Split(raw, ",")
	var constraints []string
	for _, chunk := range chunks {
		constraint := strings.TrimSpace(chunk)
		if constraint != "" {
			constraints = append(constraints, constraint)
		}
	}
	return constraints
}

func writeCombinedConfiguration(stacks []string, prefixes map[string]string, rootAbs, mergedDir string) (providerRequirements, error) {
	if len(stacks) == 0 {
		return nil, fmt.Errorf("no stacks to render")
	}

	seenVariables := make(map[string]bool)
	requiredProviders := make(providerRequirements)
	seenProviderBlocks := make(map[string]struct{})

	var builder strings.Builder
	for _, stackDir := range stacks {
		prefix := prefixes[stackDir]
		if prefix == "" {
			prefix = sanitizeIdentifier(filepath.Base(stackDir))
		}

		stackBody, stackProviders, err := renderStackConfiguration(stackDir, prefix, seenVariables, seenProviderBlocks)
		if err != nil {
			rel, relErr := filepath.Rel(rootAbs, stackDir)
			if relErr != nil {
				rel = stackDir
			}
			return nil, fmt.Errorf("rendering stack %s: %w", rel, err)
		}

		for name, req := range stackProviders {
			if existing, ok := requiredProviders[name]; ok {
				existing.merge(name, req)
				continue
			}
			requiredProviders[name] = req
		}

		if strings.TrimSpace(stackBody) == "" {
			continue
		}

		rel, err := filepath.Rel(rootAbs, stackDir)
		if err != nil {
			rel = stackDir
		}

		builder.WriteString(fmt.Sprintf("# --- Stack %s (%s) ---\n", prefix, rel))
		builder.WriteString(stackBody)
		if !strings.HasSuffix(stackBody, "\n") {
			builder.WriteString("\n")
		}
		builder.WriteString("\n")
	}

	if builder.Len() == 0 {
		return requiredProviders, fmt.Errorf("no Terraform configuration generated")
	}

	configPath := filepath.Join(mergedDir, "super.tf")
	if err := os.WriteFile(configPath, []byte(builder.String()), 0o644); err != nil {
		return requiredProviders, err
	}

	fmt.Printf("[✓] Wrote combined configuration to %s\n", configPath)
	return requiredProviders, nil
}

func renderStackConfiguration(stackDir, prefix string, seenVariables map[string]bool, seenProviders map[string]struct{}) (string, providerRequirements, error) {
	files, err := loadTerraformFiles(stackDir)
	if err != nil {
		return "", nil, err
	}
	if len(files) == 0 {
		return "", nil, nil
	}

	parsed := make([]*hclwrite.File, 0, len(files))
	ctx := newRenameContext()
	stackProviders := make(providerRequirements)

	for _, path := range files {
		src, err := os.ReadFile(path)
		if err != nil {
			return "", nil, err
		}

		file, diags := hclwrite.ParseConfig(src, path, hcl.InitialPos)
		if diags.HasErrors() {
			return "", nil, fmt.Errorf("parse %s: %s", path, diags.Error())
		}

		collectRenameRules(file.Body(), prefix, ctx, false)
		parsed = append(parsed, file)
	}

	for _, file := range parsed {
		rewriteBodyReferences(file.Body(), ctx.rules)
		if err := cleanupTerraformBlocks(file.Body(), stackProviders, seenProviders); err != nil {
			return "", nil, err
		}
		removeDuplicateVariables(file.Body(), seenVariables)
	}

	var builder strings.Builder
	for idx, file := range parsed {
		content := bytes.TrimSpace(file.Bytes())
		if len(content) == 0 {
			continue
		}
		builder.Write(content)
		builder.WriteString("\n")
		if idx != len(parsed)-1 {
			builder.WriteString("\n")
		}
	}

	return builder.String(), stackProviders, nil
}

type variableValue struct {
	tokens hclwrite.Tokens
	source string
}

func collectVariableValues(root, environment string, stacks []string) (map[string]variableValue, int, error) {
	result := make(map[string]variableValue)
	var sourcesUsed int

	sources := []struct {
		path        string
		description string
	}{
		{
			path:        filepath.Join(root, "globals.tfvars"),
			description: "globals.tfvars",
		},
		{
			path:        filepath.Join(root, "environment", fmt.Sprintf("%s.tfvars", environment)),
			description: fmt.Sprintf("environment/%s.tfvars", environment),
		},
	}

	for _, stackDir := range stacks {
		rel, err := filepath.Rel(root, stackDir)
		if err != nil {
			rel = stackDir
		}
		tfvarsPath := filepath.Join(root, rel, "tfvars", fmt.Sprintf("%s.tfvars", environment))
		sources = append(sources, struct {
			path        string
			description string
		}{
			path:        tfvarsPath,
			description: fmt.Sprintf("%s/tfvars/%s.tfvars", rel, environment),
		})
	}

	for _, src := range sources {
		vars, err := loadTFVarsFile(src.path)
		if err != nil {
			return nil, sourcesUsed, fmt.Errorf("read tfvars %s: %w", src.path, err)
		}
		if len(vars) == 0 {
			continue
		}
		sourcesUsed++
		mergeVariableTokens(result, vars, src.description)
	}

	return result, sourcesUsed, nil
}

func loadTFVarsFile(path string) (map[string]hclwrite.Tokens, error) {
	stat, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]hclwrite.Tokens{}, nil
		}
		return nil, err
	}
	if stat.IsDir() {
		return map[string]hclwrite.Tokens{}, nil
	}

	src, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	file, diags := hclwrite.ParseConfig(src, path, hcl.InitialPos)
	if diags.HasErrors() {
		return nil, fmt.Errorf("parse error: %s", diags.Error())
	}

	result := make(map[string]hclwrite.Tokens)
	for name, attr := range file.Body().Attributes() {
		result[name] = copyTokens(attr.Expr().BuildTokens(nil))
	}
	return result, nil
}

func mergeVariableTokens(dest map[string]variableValue, incoming map[string]hclwrite.Tokens, source string) {
	for name, tokens := range incoming {
		if current, exists := dest[name]; exists {
			if tokensEqual(current.tokens, tokens) {
				continue
			}
			fmt.Printf("[!] Warning: variable %q from %s overrides value from %s\n", name, source, current.source)
		}
		dest[name] = variableValue{
			tokens: copyTokens(tokens),
			source: source,
		}
	}
}

func writeTFVarsFile(path string, vars map[string]variableValue) error {
	file := hclwrite.NewEmptyFile()
	body := file.Body()

	names := make([]string, 0, len(vars))
	for name := range vars {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		value := vars[name]
		body.SetAttributeRaw(name, copyTokens(value.tokens))
	}

	return os.WriteFile(path, file.Bytes(), 0o644)
}
func loadTerraformFiles(dir string) ([]string, error) {
	var files []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}

		if d.IsDir() {
			if d.Name() == ".terraform" {
				return filepath.SkipDir
			}
			return nil
		}

		if filepath.Ext(path) == ".tf" {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	sort.Strings(files)
	return files, nil
}

func collectRenameRules(body *hclwrite.Body, prefix string, ctx *renameContext, insideModule bool) {
	if ctx == nil {
		return
	}

	for _, block := range body.Blocks() {
		switch block.Type() {
		case "resource":
			labels := block.Labels()
			if len(labels) >= 2 {
				if insideModule {
					break
				}
				resType := labels[0]
				oldName := labels[1]
				newName := prefixSegment(prefix, oldName)
				if newName != oldName {
					block.SetLabels([]string{resType, newName})
					ctx.addRule([]string{resType, oldName}, []string{resType, newName})
					ctx.addRule([]string{"resource", resType, oldName}, []string{"resource", resType, newName})
				}
			}
		case "data":
			labels := block.Labels()
			if len(labels) >= 2 {
				if insideModule {
					break
				}
				dataType := labels[0]
				oldName := labels[1]
				newName := prefixSegment(prefix, oldName)
				if newName != oldName {
					block.SetLabels([]string{dataType, newName})
					ctx.addRule([]string{"data", dataType, oldName}, []string{"data", dataType, newName})
				}
			}
		case "module":
			labels := block.Labels()
			if len(labels) >= 1 {
				if insideModule {
					break
				}
				oldName := labels[0]
				newName := prefixSegment(prefix, oldName)
				if newName != oldName {
					block.SetLabels([]string{newName})
					ctx.addRule([]string{"module", oldName}, []string{"module", newName})
				}
			}
		case "output":
			labels := block.Labels()
			if len(labels) >= 1 {
				if insideModule {
					break
				}
				oldName := labels[0]
				newName := prefixSegment(prefix, oldName)
				if newName != oldName {
					block.SetLabels([]string{newName})
				}
			}
		case "locals":
			if !insideModule {
				renameLocalAttributes(block.Body(), prefix, ctx)
			}
		}

		nextInside := insideModule || block.Type() == "module"
		collectRenameRules(block.Body(), prefix, ctx, nextInside)
	}
}

func renameLocalAttributes(body *hclwrite.Body, prefix string, ctx *renameContext) {
	attrs := body.Attributes()
	if len(attrs) == 0 {
		return
	}

	names := make([]string, 0, len(attrs))
	for name := range attrs {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		newName := prefixSegment(prefix, name)
		if newName == name {
			continue
		}
		body.RenameAttribute(name, newName)
		ctx.addRule([]string{"local", name}, []string{"local", newName})
	}
}

func rewriteBodyReferences(body *hclwrite.Body, rules []renameRule) {
	if len(rules) == 0 {
		return
	}

	for _, attr := range body.Attributes() {
		expr := attr.Expr()
		for _, rule := range rules {
			expr.RenameVariablePrefix(rule.search, rule.replacement)
		}
	}

	for _, block := range body.Blocks() {
		rewriteBodyReferences(block.Body(), rules)
	}
}

func copyTokens(tokens hclwrite.Tokens) hclwrite.Tokens {
	if tokens == nil {
		return nil
	}
	dup := make(hclwrite.Tokens, len(tokens))
	for i, tok := range tokens {
		if tok == nil {
			continue
		}
		copyTok := *tok
		dup[i] = &copyTok
	}
	return dup
}

func tokensEqual(a, b hclwrite.Tokens) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		ta := a[i]
		tb := b[i]
		if ta == nil || tb == nil {
			if ta != tb {
				return false
			}
			continue
		}
		if ta.Type != tb.Type || ta.SpacesBefore != tb.SpacesBefore || !bytes.Equal(ta.Bytes, tb.Bytes) {
			return false
		}
	}
	return true
}

func cleanupTerraformBlocks(body *hclwrite.Body, providers providerRequirements, seenProviders map[string]struct{}) error {
	blocks := body.Blocks()
	for _, block := range blocks {
		switch block.Type() {
		case "terraform":
			if err := consumeTerraformBlock(block, providers); err != nil {
				return err
			}
			body.RemoveBlock(block)
			continue
		case "resource":
			ensureLifecycleIgnoresTags(block)
		case "provider":
			keep := registerProviderBlock(block, seenProviders)
			if !keep {
				body.RemoveBlock(block)
				continue
			}
			removeProviderTagDefaults(block)
		}
		if err := cleanupTerraformBlocks(block.Body(), providers, seenProviders); err != nil {
			return err
		}
	}
	return nil
}

func consumeTerraformBlock(block *hclwrite.Block, providers providerRequirements) error {
	if providers == nil {
		return nil
	}

	for _, nested := range block.Body().Blocks() {
		if nested.Type() != "required_providers" {
			continue
		}
		attrs := nested.Body().Attributes()
		names := make([]string, 0, len(attrs))
		for name := range attrs {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			attr := attrs[name]
			if attr == nil {
				continue
			}
			if err := providers.merge(name, attr.Expr().BuildTokens(nil)); err != nil {
				return fmt.Errorf("merge required provider %q: %w", name, err)
			}
		}
	}
	return nil
}

func removeProviderTagDefaults(block *hclwrite.Block) {
	if block == nil || block.Type() != "provider" {
		return
	}

	body := block.Body()
	for _, attr := range []string{"default_tags", "tags", "tags_all"} {
		if body.GetAttribute(attr) != nil {
			body.RemoveAttribute(attr)
		}
	}

	for _, nested := range body.Blocks() {
		if nested.Type() == "default_tags" {
			body.RemoveBlock(nested)
		}
	}
}

func registerProviderBlock(block *hclwrite.Block, seen map[string]struct{}) bool {
	if seen == nil || block == nil {
		return true
	}

	labels := block.Labels()
	if len(labels) == 0 {
		return true
	}

	providerType := labels[0]
	body := block.Body()
	alias := attributeExprString(body.GetAttribute("alias"))
	region := attributeExprString(body.GetAttribute("region"))

	key := fmt.Sprintf("%s|%s|%s", providerType, alias, region)
	if _, exists := seen[key]; exists {
		fmt.Printf("[i] Skipping duplicate provider %q (alias=%s, region=%s)\n", providerType, alias, region)
		return false
	}

	seen[key] = struct{}{}
	return true
}

func attributeExprString(attr *hclwrite.Attribute) string {
	if attr == nil {
		return ""
	}
	tokens := attr.Expr().BuildTokens(nil)
	return strings.TrimSpace(tokensToString(tokens))
}

func removeDuplicateVariables(body *hclwrite.Body, seen map[string]bool) {
	if seen == nil {
		return
	}

	for _, block := range body.Blocks() {
		if block.Type() != "variable" {
			continue
		}
		labels := block.Labels()
		if len(labels) == 0 {
			continue
		}
		name := labels[0]
		if seen[name] {
			body.RemoveBlock(block)
			continue
		}
		seen[name] = true
	}
}

func ensureLifecycleIgnoresTags(block *hclwrite.Block) {
	if block == nil || block.Type() != "resource" {
		return
	}

	labels := block.Labels()
	if len(labels) == 0 {
		return
	}
	resourceType := labels[0]
	if !strings.HasPrefix(resourceType, "aws_") || shouldSkipTagLifecycle(resourceType) {
		return
	}

	body := block.Body()
	wasEmpty := len(body.Attributes()) == 0 && len(body.Blocks()) == 0
	var lifecycle *hclwrite.Block
	for _, nested := range body.Blocks() {
		if nested.Type() == "lifecycle" {
			lifecycle = nested
			break
		}
	}

	if lifecycle == nil {
		if wasEmpty {
			body.AppendNewline()
		}
		lifecycle = body.AppendNewBlock("lifecycle", nil)
	}

	lifecycleBody := lifecycle.Body()
	attr := lifecycleBody.GetAttribute("ignore_changes")
	targetAttrs := []string{"tags", "tags_all"}
	if attr == nil {
		addIgnoreChangesAttribute(lifecycleBody, targetAttrs)
		return
	}

	current := strings.TrimSpace(tokensToString(attr.Expr().BuildTokens(nil)))
	if current == "" {
		addIgnoreChangesAttribute(lifecycleBody, targetAttrs)
		return
	}

	updated := current
	for _, name := range targetAttrs {
		updated = extendIgnoreChangesExpression(updated, name)
	}

	if updated == "" || updated == current {
		return
	}

	tokens, err := tokensForExpression(updated)
	if err != nil {
		fmt.Printf("[!] Warning: failed to parse ignore_changes expression, overriding: %v\n", err)
		tokens, err = tokensForExpression("[" + strings.Join(targetAttrs, ", ") + "]")
		if err != nil {
			return
		}
	}
	lifecycleBody.SetAttributeRaw("ignore_changes", tokens)

	if wasEmpty {
		body.AppendNewline()
	}
}

func addIgnoreChangesAttribute(body *hclwrite.Body, attrs []string) {
	if len(attrs) == 0 {
		return
	}
	expr := "[" + strings.Join(attrs, ", ") + "]"
	tokens, err := tokensForExpression(expr)
	if err != nil {
		fmt.Printf("[!] Warning: unable to build ignore_changes expression: %v\n", err)
		return
	}
	body.SetAttributeRaw("ignore_changes", tokens)
}

func extendIgnoreChangesExpression(current, attr string) string {
	if current == "" {
		return "[" + attr + "]"
	}

	if containsIgnoreAttr(current, attr) {
		return current
	}

	src := fmt.Sprintf("ignore = %s", current)
	file, diags := hclsyntax.ParseConfig([]byte(src), "ignore.hcl", hcl.InitialPos)
	if !diags.HasErrors() {
		if body, ok := file.Body.(*hclsyntax.Body); ok {
			if attribute, ok := body.Attributes["ignore"]; ok {
				if tuple, ok := attribute.Expr.(*hclsyntax.TupleConsExpr); ok {
					srcBytes := []byte(src)
					values := make([]string, 0, len(tuple.Exprs)+1)
					for _, expr := range tuple.Exprs {
						value := strings.TrimSpace(extractExpression(expr, srcBytes))
						if value == "" {
							continue
						}
						values = append(values, value)
					}
					for _, existing := range values {
						if existing == attr {
							return current
						}
					}
					values = append(values, attr)
					return "[" + strings.Join(values, ", ") + "]"
				}
			}
		}
	}

	return fmt.Sprintf("concat(%s, [%s])", current, attr)
}

func shouldSkipTagLifecycle(resourceType string) bool {
	_, skip := tagLifecycleSkipTypes[resourceType]
	return skip
}

func containsIgnoreAttr(expr, attr string) bool {
	fields := strings.FieldsFunc(expr, func(r rune) bool {
		switch r {
		case '[', ']', ',', ' ', '\t', '\n', '\r', '(', ')':
			return true
		default:
			return false
		}
	})
	for _, field := range fields {
		if field == attr {
			return true
		}
	}
	return false
}

func ensureLifecycleIgnoresTagsInBody(body *hclwrite.Body) {
	if body == nil {
		return
	}
	for _, block := range body.Blocks() {
		if block.Type() == "resource" {
			ensureLifecycleIgnoresTags(block)
		}
		ensureLifecycleIgnoresTagsInBody(block.Body())
	}
}

func patchModuleResourceLifecycle(superplanDir string) error {
	modulesDir := filepath.Join(superplanDir, ".terraform", "modules")
	info, err := os.Stat(modulesDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	if !info.IsDir() {
		return nil
	}

	var updated int
	err = filepath.WalkDir(modulesDir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}
		if filepath.Ext(path) != ".tf" {
			return nil
		}

		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}

		file, diags := hclwrite.ParseConfig(src, path, hcl.InitialPos)
		if diags.HasErrors() {
			return fmt.Errorf("parse module config %s: %s", path, diags.Error())
		}

		ensureLifecycleIgnoresTagsInBody(file.Body())

		newContent := file.Bytes()
		if bytes.Equal(src, newContent) {
			return nil
		}

		if err := os.WriteFile(path, newContent, 0o644); err != nil {
			return fmt.Errorf("write module config %s: %w", path, err)
		}
		updated++
		return nil
	})
	if err != nil {
		return err
	}

	if updated > 0 {
		fmt.Printf("[i] Applied lifecycle tag ignore to %d module files\n", updated)
	}
	return nil
}

func cleanupSuperplanArtifacts(mergedDir, planPath string, keep bool) error {
	if keep {
		return nil
	}
	if err := os.RemoveAll(mergedDir); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove merged configuration: %w", err)
	}
	if err := os.Remove(planPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove plan file: %w", err)
	}
	return nil
}

func ensureLocalBackend(dir string, stateProviders map[string]string, configProviders providerRequirements) error {
	mainTFPath := filepath.Join(dir, "main.tf")

	file := hclwrite.NewEmptyFile()
	body := file.Body()

	tfBlock := body.AppendNewBlock("terraform", nil)
	tfBody := tfBlock.Body()
	tfBody.AppendNewBlock("backend", []string{"local"})

	union := make(map[string]struct{})
	for name := range configProviders {
		union[name] = struct{}{}
	}
	for name := range stateProviders {
		union[name] = struct{}{}
	}

	if len(union) > 0 {
		rpBlock := tfBody.AppendNewBlock("required_providers", nil)
		rpBody := rpBlock.Body()
		names := make([]string, 0, len(union))
		for name := range union {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			if req, ok := configProviders[name]; ok {
				tokens, err := req.tokens()
				if err != nil {
					return fmt.Errorf("render required provider %q: %w", name, err)
				}
				rpBody.SetAttributeRaw(name, tokens)
				continue
			}
			source := stateProviders[name]
			rpBody.SetAttributeValue(name, cty.ObjectVal(map[string]cty.Value{
				"source": cty.StringVal(source),
			}))
		}
	}

	return os.WriteFile(mainTFPath, file.Bytes(), 0o644)
}

type summaryContext struct {
	StackInfos        map[string]*stackMetadata
	DependenciesByRel map[string][]string
	DependentsByRel   map[string][]string
	PrefixToStack     map[string]string
	Environment       string
	AccountID         string
	TerraformVersion  string
	GeneratedAt       time.Time
}

func buildSuperplanSummary(plan *tfjson.Plan, ctx summaryContext) superplanSummary {
	if plan == nil {
		plan = &tfjson.Plan{}
	}

	stackSummaries := make(map[string]stackChangeSummary, len(ctx.StackInfos))
	for rel, info := range ctx.StackInfos {
		deps := uniqueSortedStrings(append([]string(nil), ctx.DependenciesByRel[rel]...))
		dependents := uniqueSortedStrings(append([]string(nil), ctx.DependentsByRel[rel]...))

		stackSummaries[rel] = stackChangeSummary{
			Stack:           rel,
			Prefix:          info.Prefix,
			Dependencies:    deps,
			DependentStacks: dependents,
		}
	}

	totals := resourceTotals{}
	for _, rc := range plan.ResourceChanges {
		if rc.Change == nil {
			continue
		}
		stackRel := identifyStackFromAddress(rc.Address, ctx.PrefixToStack)
		if stackRel == "" {
			continue
		}
		summary := stackSummaries[stackRel]
		for _, action := range rc.Change.Actions {
			switch action {
			case tfjson.ActionCreate:
				summary.Adds++
				totals.Adds++
			case tfjson.ActionUpdate:
				summary.Changes++
				totals.Changes++
			case tfjson.ActionDelete:
				summary.Destroys++
				totals.Destroys++
			}
		}
		if summary.Adds+summary.Changes+summary.Destroys > 0 {
			summary.HasChanges = true
			summary.Reason = "direct"
		}
		stackSummaries[stackRel] = summary
	}

	changedStacks := make(map[string]struct{}, len(stackSummaries))
	for rel, summary := range stackSummaries {
		if summary.HasChanges {
			changedStacks[rel] = struct{}{}
		}
	}
	for rel, summary := range stackSummaries {
		if summary.HasChanges {
			continue
		}
		for _, dep := range summary.Dependencies {
			if _, ok := changedStacks[dep]; ok {
				summary.Reason = "dependency"
				stackSummaries[rel] = summary
				break
			}
		}
	}

	stackCount := len(stackSummaries)
	stacksWithChanges := 0
	for rel, summary := range stackSummaries {
		if summary.HasChanges {
			stacksWithChanges++
		}
		// ensure prefix populated even if not set earlier
		if summary.Prefix == "" {
			if info := ctx.StackInfos[rel]; info != nil {
				summary.Prefix = info.Prefix
				stackSummaries[rel] = summary
			}
		}
	}

	return superplanSummary{
		GeneratedAt:       ctx.GeneratedAt,
		Environment:       ctx.Environment,
		AccountID:         ctx.AccountID,
		TerraformVersion:  ctx.TerraformVersion,
		TotalStacks:       stackCount,
		StacksWithChanges: stacksWithChanges,
		ResourceTotals:    totals,
		Stacks:            stackSummaries,
	}
}

func identifyStackFromAddress(address string, prefixToStack map[string]string) string {
	if address == "" {
		return ""
	}
	parts := splitAddressTokens(address)
	for _, part := range parts {
		for prefix, stack := range prefixToStack {
			if strings.HasPrefix(part, prefix+"_") || part == prefix {
				return stack
			}
		}
	}
	return ""
}

func splitAddressTokens(address string) []string {
	var tokens []string
	current := strings.Builder{}
	for _, r := range address {
		switch r {
		case '.', '[', ']', '"':
			if current.Len() > 0 {
				tokens = append(tokens, current.String())
				current.Reset()
			}
		default:
			current.WriteRune(r)
		}
	}
	if current.Len() > 0 {
		tokens = append(tokens, current.String())
	}
	return tokens
}

func deriveTerraformVersion(explicit string, plan *tfjson.Plan) string {
	if explicit != "" {
		return explicit
	}
	if plan != nil && plan.TerraformVersion != "" {
		return plan.TerraformVersion
	}
	return "unknown"
}

func uniqueSortedStrings(items []string) []string {
	if len(items) == 0 {
		return []string{}
	}
	sort.Strings(items)
	result := make([]string, 0, len(items))
	var last string
	for i, item := range items {
		if i == 0 || item != last {
			result = append(result, item)
			last = item
		}
	}
	return result
}
