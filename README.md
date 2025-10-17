# Terraform Wrapper

`terraform-wrapper` is an orchestration layer that coordinates Terraform stacks across an environment. It discovers stack dependencies, resolves compatible Terraform versions, executes operations in dependency order, and produces a consolidated “superplan” for review.

## Key Capabilities

- **Terraform version management** – inspects every stack’s `terraform.required_version`, prefers a compatible system binary, and installs a matching version on demand while writing a `.terraform-version.lock.json`.
- **Dependency-aware execution** – models stacks through `dependencies.json` files and runs plans/applies/destroys respecting topological order.
- **Parallel orchestration** – processes independent stacks concurrently with consistent logging and progress feedback.
- **Superplan generation** – pulls state for every stack, rewrites resources (omitting tag noise), and emits an aggregated plan for environment-wide change review.
- **S3-based orchestration locks** – uses object-lock semantics to prevent overlapping environment operations.

## Requirements

- Go 1.24 or newer
- Access to the target AWS account (for state backends and optional bootstrap)
- Terraform installed locally when `TFWRAPPER_USE_SYSTEM_TERRAFORM=true` is set

## Building

```bash
make build
```

The build embeds the first eight characters of the latest Git commit into the CLI version string (`terraform-wrapper --version`).

## Installation

```bash
sudo make install
```

Installs the binary to `/usr/local/bin`. Set `GOBIN` before running if you prefer a different destination.

## Usage

Help is available via:

```bash
terraform-wrapper --help
terraform-wrapper plan --help
```

Common commands:

| Command                        | Description                                              |
| ----------------------------- | -------------------------------------------------------- |
| `terraform-wrapper init --stack=<path>` | Initialise a specific stack.                      |
| `terraform-wrapper plan --stack=<path>` | Run an individual stack plan.                     |
| `terraform-wrapper apply --stack=<path>` | Apply a stack with auto-approval configured.      |
| `terraform-wrapper plan-all`  | Generate the dependency-aware superplan.                 |
| `terraform-wrapper apply-all` | Apply every stack in dependency order.                   |

### Terraform Version Resolution

Environment variables alter how binaries are resolved:

- `TFWRAPPER_USE_SYSTEM_TERRAFORM=true` – always use the system Terraform binary.
- `TFWRAPPER_FORCE_INSTALL=true` – install a compatible version even if the system meets requirements.
- `TFWRAPPER_DISABLE_INSTALL=true` – fail if no compatible system binary exists.

Each run updates `.terraform-version.lock.json` to preserve the binary that was executed.

### Controlling Refresh Behaviour

By default the wrapper refreshes state before every plan. Disable refresh to speed up repeated plans against static environments:

```bash
terraform-wrapper plan --stack core-services/network --refresh=false
```

The flag also applies to `plan-all` and cached plan generation.

## Stack Layout Requirements

Every stack directory should contain a `dependencies.json` file describing upstream relationships. See `docs/architecture/adr-010.md` for the schema and examples.

## Bootstrap

For new environments, the `bootstrap` command temporarily disables the local backend, applies the state bootstrap stack, and re-enables remote state:

```bash
terraform-wrapper bootstrap \
  --root ./infrastructure \
  --environment staging \
  --region eu-west-2
```

## Development Workflow

- `make test` – run the full test suite.
- `make test-unit` – run package-level unit tests.
- `make fmt` – format Go files.
- `make clean` – remove `.terraform` directories and lock files.

Architecture decisions, superplan behaviour, and tagging considerations are documented under `docs/architecture/` and `docs/updating-tagless-resources.md`.

## Contributing

1. Fork and clone the repository.
2. Create feature branches off `main`.
3. Run `make test` to validate changes.
4. Update relevant ADRs or docs when altering functionality.

## License

MIT
