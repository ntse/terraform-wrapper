package superplan

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclwrite"
)

func TestPrefixResourcesAndOutputs(t *testing.T) {
	state := map[string]interface{}{
		"resources": []interface{}{
			map[string]interface{}{
				"name":     "main",
				"address":  "aws_s3_bucket.state",
				"provider": "provider[\"registry.terraform.io/hashicorp/aws\"]",
			},
			map[string]interface{}{
				"name":    "from_module",
				"address": "module.child.aws_s3_bucket.child",
			},
		},
		"outputs": map[string]interface{}{
			"bucket": map[string]interface{}{
				"value": "arn",
			},
		},
	}

	n, err := prefixResources(state, "state")
	if err != nil {
		t.Fatalf("prefix resources: %v", err)
	}
	if n != 2 {
		t.Fatalf("expected 2 resources, got %d", n)
	}

	resources := state["resources"].([]interface{})
	root := resources[0].(map[string]interface{})
	if root["name"] != "state_main" {
		t.Fatalf("name not prefixed: %+v", root["name"])
	}
	if root["address"] != "aws_s3_bucket.state_state" {
		t.Fatalf("address not prefixed: %s", root["address"])
	}

	moduleRes := resources[1].(map[string]interface{})
	if moduleRes["address"] != "module.state_child.aws_s3_bucket.child" {
		t.Fatalf("module address unexpected: %s", moduleRes["address"])
	}

	outCount := prefixOutputs(state, "state")
	if outCount != 1 {
		t.Fatalf("expected 1 output, got %d", outCount)
	}
	outputs := state["outputs"].(map[string]interface{})
	if _, ok := outputs["state_bucket"]; !ok {
		t.Fatalf("prefixed output missing: %#v", outputs)
	}
}

func TestCollectProviders(t *testing.T) {
	state := map[string]interface{}{
		"resources": []interface{}{
			map[string]interface{}{
				"provider": "provider[\"registry.terraform.io/hashicorp/aws\"]",
			},
			map[string]interface{}{
				"provider": "provider[\"registry.terraform.io/hashicorp/tls\"]",
			},
		},
	}

	providers := map[string]string{}
	collectProviders(state, providers)

	if providers["aws"] != "registry.terraform.io/hashicorp/aws" {
		t.Fatalf("provider aws not collected: %#v", providers)
	}
	if providers["tls"] != "registry.terraform.io/hashicorp/tls" {
		t.Fatalf("provider tls not collected")
	}
}

func TestEnsureLocalBackendWritesProviders(t *testing.T) {
	dir := t.TempDir()
	providers := map[string]string{
		"aws": "registry.terraform.io/hashicorp/aws",
		"tls": "registry.terraform.io/hashicorp/tls",
	}

	if err := ensureLocalBackend(dir, providers, nil); err != nil {
		t.Fatalf("ensureLocalBackend: %v", err)
	}

	content, err := os.ReadFile(filepath.Join(dir, "main.tf"))
	if err != nil {
		t.Fatalf("read main.tf: %v", err)
	}

	code := string(content)
	for name, source := range providers {
		if !containsAll(code, name, source) {
			t.Fatalf("provider %s with source %s missing from main.tf\n%s", name, source, code)
		}
	}
}

func TestProviderRequirementsMerge(t *testing.T) {
	first, err := tokensForTest(`
{
  source  = "hashicorp/aws"
  version = "~> 5.0"
  configuration_aliases = [aws.us_east_1]
}`)
	if err != nil {
		t.Fatalf("tokensForTest first: %v", err)
	}

	second, err := tokensForTest(`
{
  version = ">= 5.2"
  configuration_aliases = [aws.us_west_2]
}`)
	if err != nil {
		t.Fatalf("tokensForTest second: %v", err)
	}

	reqs := make(providerRequirements)
	if err := reqs.merge("aws", first); err != nil {
		t.Fatalf("merge first: %v", err)
	}
	if err := reqs.merge("aws", second); err != nil {
		t.Fatalf("merge second: %v", err)
	}

	req := reqs["aws"]
	if req == nil {
		t.Fatalf("expected merged requirement for aws")
	}
	if !req.HasSource || req.Source != "hashicorp/aws" {
		t.Fatalf("unexpected source: %+v", req.Source)
	}
	if got := req.versionString(); got != ">= 5.2, ~> 5.0" {
		t.Fatalf("unexpected version string: %s", got)
	}

	aliases := req.aliasesList()
	if len(aliases) != 2 || aliases[0] != "aws.us_east_1" || aliases[1] != "aws.us_west_2" {
		t.Fatalf("unexpected aliases: %#v", aliases)
	}

	rendered, err := req.tokens()
	if err != nil {
		t.Fatalf("render tokens: %v", err)
	}
	output := tokensToString(rendered)
	if !containsAll(output, "~> 5.0", ">= 5.2", "aws.us_west_2") {
		t.Fatalf("rendered tokens missing expected content:\n%s", output)
	}
}

func TestStripTagAttributesFromState(t *testing.T) {
	state := map[string]interface{}{
		"resources": []interface{}{
			map[string]interface{}{
				"instances": []interface{}{
					map[string]interface{}{
						"attributes": map[string]interface{}{
							"region":       "eu-west-2",
							"tags":         map[string]interface{}{"Env": "dev"},
							"tags_all":     map[string]interface{}{"Env": "dev"},
							"default_tags": map[string]interface{}{"tags": map[string]interface{}{"Env": "dev"}},
						},
						"values": map[string]interface{}{
							"tags":     map[string]interface{}{"Env": "dev"},
							"tags_all": map[string]interface{}{"Env": "dev"},
						},
						"after_unknown": map[string]interface{}{
							"tags_all": true,
							"id":       true,
						},
						"before_sensitive": map[string]interface{}{
							"tags": map[string]interface{}{"Env": "dev"},
						},
						"after_sensitive": map[string]interface{}{
							"tags_all": map[string]interface{}{"Env": "dev"},
							"other":    "value",
						},
					},
				},
			},
			map[string]interface{}{
				"attributes": map[string]interface{}{
					"name": "example",
					"tags": map[string]interface{}{"Env": "dev"},
				},
			},
		},
	}

	stripTagAttributesFromState(state)

	resources := state["resources"].([]interface{})
	firstResource := resources[0].(map[string]interface{})
	instance := firstResource["instances"].([]interface{})[0].(map[string]interface{})
	attrs := instance["attributes"].(map[string]interface{})
	if val, ok := attrs["tags"].(map[string]interface{}); !ok || len(val) != 0 {
		t.Fatalf("tags attribute not cleared to empty map in instance attributes: %#v", attrs["tags"])
	}
	if val, ok := attrs["tags_all"].(map[string]interface{}); !ok || len(val) != 0 {
		t.Fatalf("tags_all attribute not cleared to empty map in instance attributes: %#v", attrs["tags_all"])
	}
	if val, ok := attrs["default_tags"].(map[string]interface{}); !ok || len(val) != 0 {
		t.Fatalf("default_tags attribute not cleared to empty map in instance attributes: %#v", attrs["default_tags"])
	}
	if attrs["region"] != "eu-west-2" {
		t.Fatalf("non-tag attribute unexpectedly modified: %#v", attrs)
	}

	values := instance["values"].(map[string]interface{})
	if val, ok := values["tags"].(map[string]interface{}); !ok || len(val) != 0 {
		t.Fatalf("tags attribute not cleared to empty map in instance values: %#v", values["tags"])
	}
	if val, ok := values["tags_all"].(map[string]interface{}); !ok || len(val) != 0 {
		t.Fatalf("tags_all attribute not cleared to empty map in instance values: %#v", values["tags_all"])
	}

	secondResource := resources[1].(map[string]interface{})
	secondAttrs := secondResource["attributes"].(map[string]interface{})
	if val, ok := secondAttrs["tags"].(map[string]interface{}); !ok || len(val) != 0 {
		t.Fatalf("tags attribute not cleared to empty map in resource attributes: %#v", secondAttrs["tags"])
	}
	if secondAttrs["name"] != "example" {
		t.Fatalf("unexpected change to non-tag attribute: %#v", secondAttrs)
	}

	unknown := instance["after_unknown"].(map[string]interface{})
	if _, ok := unknown["tags_all"]; ok {
		t.Fatalf("after_unknown retains tags_all flag: %#v", unknown)
	}
	if unknown["id"] != true {
		t.Fatalf("non-tag unknown flag modified: %#v", unknown)
	}

	beforeSensitive := instance["before_sensitive"].(map[string]interface{})
	if val, ok := beforeSensitive["tags"].(map[string]interface{}); !ok || len(val) != 0 {
		t.Fatalf("before_sensitive tags not cleared to empty map: %#v", beforeSensitive["tags"])
	}

	afterSensitive := instance["after_sensitive"].(map[string]interface{})
	if val, ok := afterSensitive["tags_all"].(map[string]interface{}); !ok || len(val) != 0 {
		t.Fatalf("after_sensitive tags_all not cleared to empty map: %#v", afterSensitive["tags_all"])
	}
	if afterSensitive["other"] != "value" {
		t.Fatalf("non-tag sensitive attribute modified: %#v", afterSensitive["other"])
	}
}

func TestCleanupTerraformBlocksRemovesDefaultTags(t *testing.T) {
	src := `
terraform {
  required_providers {
    aws = {
      source  = "hashicorp/aws"
      version = "~> 5.0"
    }
  }
}

provider "aws" {
  region = "eu-west-2"
  default_tags {
    tags = {
      Env = "dev"
    }
  }
}
`
	file, diags := hclwrite.ParseConfig([]byte(src), "test.hcl", hcl.InitialPos)
	if diags.HasErrors() {
		t.Fatalf("parse config: %s", diags.Error())
	}

	providers := make(providerRequirements)
	seen := make(map[string]struct{})
	if err := cleanupTerraformBlocks(file.Body(), providers, seen); err != nil {
		t.Fatalf("cleanupTerraformBlocks: %v", err)
	}

	if _, ok := providers["aws"]; !ok {
		t.Fatalf("expected aws provider requirement to be captured")
	}

	rendered := string(file.Bytes())
	if strings.Contains(rendered, "default_tags") {
		t.Fatalf("default_tags block still present:\n%s", rendered)
	}
	if strings.Contains(rendered, "terraform") {
		t.Fatalf("terraform block should be removed:\n%s", rendered)
	}

	reqTokens, err := providers["aws"].tokens()
	if err != nil {
		t.Fatalf("render provider tokens: %v", err)
	}
	reqRendered := tokensToString(reqTokens)
	if !containsAll(reqRendered, "hashicorp/aws", "~> 5.0") {
		t.Fatalf("unexpected provider requirement tokens:\n%s", reqRendered)
	}
}

func TestCleanupTerraformBlocksDeduplicatesProviders(t *testing.T) {
	src := `
provider "aws" {
  region = "eu-west-2"
}

provider "aws" {
  region = "eu-west-2"
}

provider "aws" {
  alias  = "us"
  region = "us-east-1"
}
`
	file, diags := hclwrite.ParseConfig([]byte(src), "dup.hcl", hcl.InitialPos)
	if diags.HasErrors() {
		t.Fatalf("parse config: %s", diags.Error())
	}

	providers := make(providerRequirements)
	seen := make(map[string]struct{})
	if err := cleanupTerraformBlocks(file.Body(), providers, seen); err != nil {
		t.Fatalf("cleanupTerraformBlocks: %v", err)
	}

	blocks := file.Body().Blocks()
	var providerCount int
	for _, block := range blocks {
		if block.Type() == "provider" {
			providerCount++
		}
	}
	if providerCount != 2 {
		t.Fatalf("expected 2 provider blocks after dedupe, got %d", providerCount)
	}

	rendered := string(file.Bytes())
	if strings.Count(rendered, `region = "eu-west-2"`) != 1 {
		t.Fatalf("duplicate default provider not removed:\n%s", rendered)
	}
	if !strings.Contains(rendered, `alias  = "us"`) {
		t.Fatalf("aliased provider should remain:\n%s", rendered)
	}
}

func TestEnsureLifecycleIgnoreTags(t *testing.T) {
	src := `
resource "aws_s3_bucket" "plain" {
  bucket = "example"
}

resource "aws_s3_bucket" "with_lifecycle" {
  lifecycle {
    prevent_destroy = true
  }
}

resource "aws_s3_bucket" "existing_ignore" {
  lifecycle {
    ignore_changes = [acl]
  }
}

resource "aws_iam_role_policy_attachment" "skip" {
  role       = "example"
  policy_arn = "arn:aws:iam::123456789012:policy/example"
}

resource "aws_kms_key" "single" {}
`
	file, diags := hclwrite.ParseConfig([]byte(src), "resource.hcl", hcl.InitialPos)
	if diags.HasErrors() {
		t.Fatalf("parse config: %s", diags.Error())
	}

	providers := make(providerRequirements)
	seen := make(map[string]struct{})
	if err := cleanupTerraformBlocks(file.Body(), providers, seen); err != nil {
		t.Fatalf("cleanupTerraformBlocks: %v", err)
	}

	resources := file.Body().Blocks()
	if len(resources) != 5 {
		t.Fatalf("expected 5 resource blocks, got %d", len(resources))
	}

	for _, res := range resources {
		lifecycleBlocks := res.Body().Blocks()
		var lifecycle *hclwrite.Block
		for _, block := range lifecycleBlocks {
			if block.Type() == "lifecycle" {
				lifecycle = block
				break
			}
		}
		if res.Labels()[0] == "aws_iam_role_policy_attachment" {
			if lifecycle != nil {
				t.Fatalf("skip resource unexpectedly gained lifecycle: %v", res.Labels())
			}
			continue
		}
		if lifecycle == nil {
			t.Fatalf("resource %v missing lifecycle block", res.Labels())
		}
		attr := lifecycle.Body().GetAttribute("ignore_changes")
		if attr == nil {
			t.Fatalf("resource %v missing ignore_changes", res.Labels())
		}
		expr := strings.TrimSpace(tokensToString(attr.Expr().BuildTokens(nil)))
		if !ignoreExprContains(expr, "tags") || !ignoreExprContains(expr, "tags_all") {
			t.Fatalf("resource %v ignore_changes missing tags/tags_all: %s", res.Labels(), expr)
		}
	}

	lifecycle := resources[2].Body().Blocks()[0]
	if attr := lifecycle.Body().GetAttribute("ignore_changes"); attr != nil {
		expr := strings.TrimSpace(tokensToString(attr.Expr().BuildTokens(nil)))
		if !(ignoreExprContains(expr, "acl") && ignoreExprContains(expr, "tags") && ignoreExprContains(expr, "tags_all")) {
			t.Fatalf("existing ignore list not preserved: %s", expr)
		}
	}

	rendered := string(file.Bytes())
	if !strings.Contains(rendered, "resource \"aws_kms_key\" \"single\" {\n  lifecycle {\n    ignore_changes = [tags, tags_all]\n  }\n}\n") {
		t.Fatalf("single-line resource not expanded correctly:\n%s", rendered)
	}
}

func tokensForTest(expr string) (hclwrite.Tokens, error) {
	source := strings.TrimSpace(expr)
	src := fmt.Sprintf("value = %s", source)
	file, diags := hclwrite.ParseConfig([]byte(src), "generated.hcl", hcl.InitialPos)
	if diags.HasErrors() {
		return nil, fmt.Errorf("parse expression: %s", diags.Error())
	}

	attr := file.Body().GetAttribute("value")
	if attr == nil {
		return nil, fmt.Errorf("missing value attribute")
	}
	return attr.Expr().BuildTokens(nil), nil
}

func containsAll(s string, parts ...string) bool {
	for _, p := range parts {
		if !strings.Contains(s, p) {
			return false
		}
	}
	return true
}

func ignoreExprContains(expr, attr string) bool {
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

func TestPatchModuleResourceLifecycle(t *testing.T) {
	dir := t.TempDir()
	modDir := filepath.Join(dir, ".terraform", "modules", "example")
	if err := os.MkdirAll(modDir, 0o755); err != nil {
		t.Fatalf("mkdir modules: %v", err)
	}

	moduleTF := `
resource "aws_lb" "this" {
  name = "example"
}

resource "aws_iam_role_policy_attachment" "skip" {
  role       = "example"
  policy_arn = "arn:aws:iam::123456789012:policy/example"
}
`
	path := filepath.Join(modDir, "main.tf")
	if err := os.WriteFile(path, []byte(moduleTF), 0o644); err != nil {
		t.Fatalf("write module tf: %v", err)
	}

	if err := patchModuleResourceLifecycle(dir); err != nil {
		t.Fatalf("patchModuleResourceLifecycle: %v", err)
	}

	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read module tf: %v", err)
	}

	updated := string(content)
	if !strings.Contains(updated, "ignore_changes") || !strings.Contains(updated, "tags") || !strings.Contains(updated, "tags_all") {
		t.Fatalf("lifecycle ignore not added to module resource:\n%s", updated)
	}

	file, diags := hclwrite.ParseConfig(content, path, hcl.InitialPos)
	if diags.HasErrors() {
		t.Fatalf("parse updated module tf: %s", diags.Error())
	}

	var skipHasLifecycle bool
	for _, block := range file.Body().Blocks() {
		if block.Type() == "resource" && len(block.Labels()) == 2 {
			if block.Labels()[0] == "aws_iam_role_policy_attachment" {
				for _, nested := range block.Body().Blocks() {
					if nested.Type() == "lifecycle" {
						skipHasLifecycle = true
					}
				}
			}
		}
	}
	if skipHasLifecycle {
		t.Fatalf("skip resource unexpectedly gained lifecycle block")
	}
}

func TestCleanupPlanArtifacts(t *testing.T) {
	dir := t.TempDir()
	planPath := filepath.Join(dir, planFileName)
	if err := os.WriteFile(planPath, []byte("plan"), 0o644); err != nil {
		t.Fatalf("write plan: %v", err)
	}
	otherPath := filepath.Join(dir, "super.tf")
	if err := os.WriteFile(otherPath, []byte("content"), 0o644); err != nil {
		t.Fatalf("write other: %v", err)
	}
	nestedDir := filepath.Join(dir, ".terraform")
	if err := os.MkdirAll(nestedDir, 0o755); err != nil {
		t.Fatalf("mkdir nested: %v", err)
	}
	if err := os.WriteFile(filepath.Join(nestedDir, "file"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write nested file: %v", err)
	}

	if err := cleanupPlanArtifacts(dir, planFileName, false); err != nil {
		t.Fatalf("cleanupPlanArtifacts: %v", err)
	}

	if _, err := os.Stat(planPath); err != nil {
		t.Fatalf("plan removed unexpectedly: %v", err)
	}
	if _, err := os.Stat(otherPath); !os.IsNotExist(err) {
		t.Fatalf("other artifact not removed: %v", err)
	}
	if _, err := os.Stat(nestedDir); !os.IsNotExist(err) {
		t.Fatalf("nested dir not removed: %v", err)
	}

	// recreate and verify keep flag preserves artifacts
	if err := os.WriteFile(otherPath, []byte("content"), 0o644); err != nil {
		t.Fatalf("rewrite other: %v", err)
	}
	if err := cleanupPlanArtifacts(dir, planFileName, true); err != nil {
		t.Fatalf("cleanup with keep: %v", err)
	}
	if _, err := os.Stat(otherPath); err != nil {
		t.Fatalf("artifact unexpectedly removed when keep=true: %v", err)
	}
}
