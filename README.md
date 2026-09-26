# ci-shift

**Shift CI/CD pipelines between providers.** `cishift` translates pipeline
configurations from one CI provider to another and checks them against each
provider's official JSON schema.

| Provider | Name | Default file |
|---|---|---|
| GitHub Actions | `github` | `.github/workflows/ci.yml` |
| GitLab CI/CD | `gitlab` | `.gitlab-ci.yml` |
| CircleCI | `circleci` | `.circleci/config.yml` |
| Azure Pipelines | `azure` | `azure-pipelines.yml` |
| Bitbucket Pipelines | `bitbucket` | `bitbucket-pipelines.yml` |

Every provider converts to and from every other one. Instead of writing
converters for each pair, each provider has a parser into a shared pipeline
model (`ir`) and an emitter back out of it:

```
 .gitlab-ci.yml ──parse──┐                ┌──emit──► .github/workflows/ci.yml
 .circleci/config.yml ───┼──► ir.Pipeline ┼──────► azure-pipelines.yml
 bitbucket-pipelines.yml ┘                └──────► ...
```

## Install

```bash
go install github.com/viniciusdc/ci-shift/cmd/cishift@latest
# or from a checkout
make build && ./cishift --help
```

## Usage

```bash
# Convert: give a file and a target (the source provider is detected)
cishift .gitlab-ci.yml --to github -o .github/workflows/ci.yml

# Check the result against the target schema; fail on lossy conversions
cishift .circleci/config.yml --to azure --check --strict

# Read from stdin
cat bitbucket-pipelines.yml | cishift - --from bitbucket --to gitlab

# Check files against their provider schema
cishift check .github/workflows/*.yml
cishift check --latest .gitlab-ci.yml   # use the newest upstream schema

# Which provider is this file for?
cishift detect some-pipeline.yml

cishift providers
```

`cishift convert <file> --to <provider>` and `cishift validate` still work
as aliases, for scripts that prefer explicit verbs.

Converted files start with a comment header naming the source. It also lists
every **warning**: anything ci-shift dropped or approximated, such as a
GitHub Marketplace action with no equivalent, or a GitLab `rules:changes`.
Warnings also go to stderr. `--strict` turns them into a non-zero exit code,
which is useful in CI.

Steps that cannot be translated, such as third-party GitHub actions,
CircleCI orbs, Azure tasks or Bitbucket pipes, become a **failing
placeholder step**. A converted pipeline therefore never silently skips
work.

## What gets translated

| Concept | Notes |
|---|---|
| Triggers | push branches/tags/paths, pull requests, schedules, manual runs |
| Jobs and dependencies | GitLab stages and Bitbucket sequences become an explicit dependency graph and back |
| Runners and images | hosted OS images, self-hosted labels, job containers |
| Services | sidecar containers (databases, caches) |
| Matrices | cartesian axes, include/exclude, GitLab `parallel:matrix`, Azure legs |
| Steps | scripts (env, working directory, shell), checkout options |
| Tool setup | `setup-node`/`UseNode@1`/..., folded into container images where the provider is container-based |
| Caches | key prefix + lockfile hashing (`hashFiles`, `checksum`, `key:files`, `Cache@2`) |
| Artifacts | upload/download between jobs (workspaces on CircleCI, automatic passing on GitLab/Bitbucket) |
| Conditions | branch/tag/event filters, `always()`/`failure()`, manual approval |
| Variables | pipeline/job env, secrets, and well-known CI variables (`$CI_COMMIT_SHA` ⇄ `${{ github.sha }}` ⇄ `$CIRCLE_SHA1` ⇄ `$(Build.SourceVersion)` ⇄ `$BITBUCKET_COMMIT`) |

Variables that a pipeline references but never defines (e.g. `$PYPI_TOKEN`
from GitLab project settings) are treated as secrets. On GitHub and Azure
they are mapped into each step's `env`, since those providers do not
expose secrets automatically.

### Known limits

- Includes and templates (`include:`, Azure `template:`, CircleCI orbs) are
  not resolved. Convert the referenced files separately.
- Provider-specific expressions that ci-shift cannot parse (e.g. GitHub
  `contains(...)` conditions or job outputs) are reported and dropped.
- Schedules on GitLab and Bitbucket live in project settings, so ci-shift
  tells you which cron expression to create there.

## Library use

```go
import (
	"github.com/viniciusdc/ci-shift/convert"
	"github.com/viniciusdc/ci-shift/schema"
)

from, _ := convert.Get("gitlab")
to, _ := convert.Get("github")
res, err := convert.Convert(data, from, to, convert.Options{Header: true})
// res.Output, res.Diagnostics, res.Pipeline (the IR)

issues, err := schema.Validate("github", res.Output, schema.Options{})
```

## Project layout

```
ir/             provider-neutral pipeline model, expressions, graph helpers
providers/*/    one parser + emitter per provider
convert/        provider registry, detection, conversion
schema/         JSON schema validation (embedded snapshots in schema/snapshots)
internal/cli/   cobra commands
cmd/cishift/    the cishift binary
internal/yamlx/ order-preserving YAML read/write helpers
testdata/       sample pipelines and golden conversion outputs
```

## Development

```bash
make test      # unit, round-trip, cross-provider golden and schema tests
make lint      # golangci-lint
make golden    # regenerate golden outputs after an intentional change
make schemas   # refresh embedded schema snapshots from upstream
```

Adding a provider means implementing `ir.Provider` (`Parse`, `Emit`,
`Detect`) in `providers/<name>`, registering it in `convert/convert.go`,
and adding its schema URL to `schema/schema.go`.
