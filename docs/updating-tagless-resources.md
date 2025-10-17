For the superplan to not show differences with all tagged resources, we must skip adding a lifecycle to ignore tags and tags_all to all resources that don't support it. For that, we maintain a list of tagless resources in `internal/superplan/run.go`. Occassionally, this list will need to be updated. 


```bash
terraform providers schema -json > schema.json # This needs to be run from inside a stack which uses the latest AWS provider.
jq -r '
  .provider_schemas["registry.terraform.io/hashicorp/aws"].resource_schemas
  | to_entries
  | map(select((.value.block.attributes.tags | not) and (.value.block.attributes.tags_all | not)))
  | .[].key
' schema.json > resources_without_tags.txt
```