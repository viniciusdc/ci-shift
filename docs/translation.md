# What gets translated

Every provider has a parser into ci-shift's shared pipeline model (the `ir`
package) and an emitter back out of it, so every provider converts to and
from every other one. This page lists what that model carries.

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

## Secrets

Variables that a pipeline references but never defines (e.g. `$PYPI_TOKEN`
from GitLab project settings) are treated as secrets. On GitHub and Azure
they are mapped into each step's `env`, since those providers do not
expose secrets automatically.

## Warnings and placeholders

Converted files start with a comment header naming the source. It also lists
every **warning**: anything ci-shift dropped or approximated, such as a
GitHub Marketplace action with no equivalent, or a GitLab `rules:changes`.
Warnings also go to stderr. `--strict` turns them into a non-zero exit code,
which is useful in CI; `--no-header` omits the header.

Steps that cannot be translated, such as third-party GitHub actions,
CircleCI orbs, Azure tasks or Bitbucket pipes, become a **failing
placeholder step**. A converted pipeline therefore never silently skips
work.

## Known limits

- Includes and templates (`include:`, Azure `template:`, CircleCI orbs) are
  not resolved. Convert the referenced files separately.
- Provider-specific expressions that ci-shift cannot parse (e.g. GitHub
  `contains(...)` conditions or job outputs) are reported and dropped.
- Schedules on GitLab and Bitbucket live in project settings, so ci-shift
  tells you which cron expression to create there.
