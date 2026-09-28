# Live provider tests

The unit and golden tests check that ci-shift produces valid YAML. These
tests check that the YAML does what the source says when a real provider
runs it.

[`conformance/ci.yml`](conformance/ci.yml) is a GitHub Actions workflow in
which each job exercises one translated feature and fails if that feature
was mistranslated. The runner in this directory converts it to GitLab,
CircleCI, Azure Pipelines and Bitbucket, pushes each result to a fresh
branch of a throwaway repository on that provider, waits for the run and
checks every job. It then deletes the branch, even when something failed.

```
conformance/ci.yml ──cishift──▶ .gitlab-ci.yml ──git push──▶ e2e/<run>-gitlab ──API──▶ jobs ✔/✘
                             ├─▶ .circleci/config.yml   (GitHub repo followed by CircleCI)
                             ├─▶ azure-pipelines.yml    (Azure Repos)
                             └─▶ bitbucket-pipelines.yml
```

Nothing here needs an account to be useful. `go test ./...` already checks,
offline, that the pipeline converts to all four targets without a single
warning (as `cishift --strict` would) and passes each target's schema. The
live run adds a provider as soon as its credentials exist; until then it
reports that provider as **skipped**, and the job stays green.

## What the conformance pipeline checks

| Job | Passes when | Fails when ci-shift gets wrong… |
|---|---|---|
| `checkout-repo` | `e2e/conformance/ci.yml` is in the working directory | checkout |
| `artifact-produce` | it writes `handoff/token.txt` and publishes it | artifact upload |
| `artifact-consume` | it runs after `artifact-produce` and reads `made-by-produce` from the artifact | `needs`, artifact passing |
| `matrix-legs` | there are exactly two legs, each with a value from `red`/`blue` | matrix expansion, `${{ matrix.* }}` |
| `env-vars` | pipeline-, job- and step-level variables have their values, and `E2E_SECRET` is non-empty (checked with `test -n`, never printed) | variable scoping, secret mapping |
| `postgres-service` | a `postgres:16` service accepts TCP connections on 5432 (`postgres`, `localhost` or `127.0.0.1`, whichever the provider uses) | service containers |
| `cache-roundtrip` | the cache restore and save steps run | cache steps |
| `on-test-branch` | it runs, because its condition matches the `e2e/` branch prefix | branch conditions |
| `never-runs` | it is skipped or not scheduled (it runs `exit 1`) | branch conditions |

Every job runs in `debian:bookworm-slim`, which meets Azure's container-job
requirements (bash, glibc, no `ENTRYPOINT`) and runs everywhere else. The
runner matches jobs by name, ignoring case and punctuation, so `matrix-legs
(red)`, `matrix_legs red` and `matrix-legs: [red]` all count as legs of
`matrix-legs`.

When you change `ci.yml`, update `Jobs` in
[`conformance/conformance.go`](conformance/conformance.go); a test keeps the two in step.

## Running it

### From GitHub Actions

**Actions › E2E › Run workflow.** The `providers` input takes a
comma-separated list; the default is all four. Each provider runs as its
own job in the `e2e` environment, and runs never overlap (they share the
throwaway repositories).

### Locally

```bash
go run ./e2e -dry-run                  # convert + validate, show what would be pushed; no network
go run ./e2e -dry-run -out /tmp/e2e    # also write the files to /tmp/e2e/<provider>/
go run ./e2e -providers gitlab         # a real run; set the provider's E2E_* variables first
```

| Flag | Default | |
|---|---|---|
| `-providers` | `$E2E_PROVIDERS`, else all four | which providers to run |
| `-timeout` | `30m` | per provider, from the push to the finished run |
| `-run-id` | `$GITHUB_RUN_ID-$GITHUB_RUN_ATTEMPT`, else `local-<time>` | test branches are `e2e/<run-id>-<provider>` |
| `-dry-run` | off | no network, no git |
| `-out` | – | with `-dry-run`, where to write the converted files |
| `-summary` | `$GITHUB_STEP_SUMMARY` | where to append the Markdown summary |

The exit code is 1 when any provider that ran failed, and 0 otherwise.
Skipped providers never fail the run.

### Reading the results

The summary goes to stdout and to the job's step summary. It has one row per
provider, with the result, a link to the provider's run and how long it
took. Below that, each provider that ran gets a table of the nine jobs,
listing what each verifies, what the provider reported, and ✅ or ❌ with the
reason. Jobs the provider reported that the pipeline does not define (setup
jobs, for example) are listed without a verdict.

| Result | Meaning |
|---|---|
| ✅ passed | every job behaved as expected and the run succeeded |
| ❌ failed | a check failed, the run failed or timed out, or setup/push failed (the reason says which) |
| ⏭️ skipped | settings are missing in the `e2e` environment; the row lists which |
| 📝 dry-run | `-dry-run`: converted and validated only |

## Setup

All settings live in a GitHub **environment** named `e2e` (**Settings ›
Environments**). Fork pull requests cannot read it, and you can add required
reviewers so that nobody runs the workflow unapproved. GitHub creates the
environment on the first run if it does not exist yet. Tokens go under
**Environment secrets** and everything else under **Environment variables**.
The workflow hands each job only its own provider's settings.

You never set `E2E_SECRET` yourself. Each run generates a random value,
stores it in the provider as a secret variable through the provider's API,
and masks it in the logs. The `env-vars` job then checks that the value
reached the script.

Credentials never touch the disk. git gets the token as an `http.extraHeader`
through `GIT_CONFIG_*` environment variables, with your git configuration
and tracing disabled, and API errors carry the response body but never the
request.

### GitLab

1. Create a **dedicated** GitLab.com account and a private project in it,
   e.g. `ci-shift-e2e`, initialized with a README. A dedicated account keeps
   the personal access token's reach small.
2. Make sure GitLab-hosted runners can run: **Settings › CI/CD › Runners ›
   Instance runners** must be enabled. The first pipeline may ask the account
   to verify its identity (email, phone and/or credit card, depending on a
   risk score). Complete it once in the UI.
3. Create a token with the **`api`** and **`write_repository`** scopes. The
   account needs the **Maintainer** role, because writing CI/CD variables
   requires it.
   - Free: a **personal access token** (**Preferences › Access tokens**).
     Project access tokens need Premium or Ultimate.
   - Premium/Ultimate: a **project access token** (**Settings › Access
     tokens**) with the Maintainer role.
4. Add to the `e2e` environment:

| Kind | Name | Value |
|---|---|---|
| secret | `E2E_GITLAB_TOKEN` | the token |
| variable | `E2E_GITLAB_PROJECT` | `<namespace>/ci-shift-e2e` |
| variable | `E2E_GITLAB_URL` | optional; a self-managed instance, default `https://gitlab.com` |

The runner creates or updates the project variable `E2E_SECRET`: masked,
and not protected, so that the `e2e/*` branches can read it.

### Bitbucket

1. Create a workspace (or use yours) and a private repository, e.g.
   `ci-shift-e2e`, with an initial commit.
2. Enable Pipelines: **Repository settings › Pipelines › Settings › Enable
   Pipelines**.
3. Create a **repository access token** (**Repository settings › Security ›
   Access tokens**; all plans have them). Give it these scopes:
   **Repositories: Write**, **Pipelines: Read** and **Pipelines: Edit
   variables** (`repository:write`, `pipeline`, `pipeline:variable`). With a
   repository access token, git uses the user name `x-token-auth` and the API
   uses Bearer auth.
   - Alternatively, use an Atlassian **API token** with scopes
     `write:repository:bitbucket`, `read:pipeline:bitbucket` and
     `admin:pipeline:bitbucket`, and also set `E2E_BITBUCKET_EMAIL`. App
     passwords no longer work: Atlassian switched them off on 2026-07-28.
4. Add to the `e2e` environment:

| Kind | Name | Value |
|---|---|---|
| secret | `E2E_BITBUCKET_TOKEN` | the token |
| variable | `E2E_BITBUCKET_WORKSPACE` | workspace slug |
| variable | `E2E_BITBUCKET_REPO` | repository slug |
| variable | `E2E_BITBUCKET_EMAIL` | only with an API token: your Atlassian account email |

The runner creates or updates the secured repository variable `E2E_SECRET`.

### Azure DevOps

1. Create an organization and a private project at
   [dev.azure.com](https://dev.azure.com), and a repository in **Repos**
   (initialize it with a README).
2. Get hosted parallelism. Private projects get no Microsoft-hosted
   parallel job until the free grant is approved: 1 job, 1,800 minutes a
   month. The current docs grant it once the organization is linked to an
   Azure subscription (**Organization settings › Billing**). Older
   organizations requested it through https://aka.ms/azpipelines-parallelism-request.
   See [Configure and pay for parallel jobs](https://learn.microsoft.com/azure/devops/pipelines/licensing/concurrent-jobs).
3. Create the pipeline once. The runner looks for a pipeline named
   `ci-shift-e2e` (or `E2E_AZURE_PIPELINE`) and tries to create it when it is
   missing, but Azure reads `azure-pipelines.yml` from the default branch
   when a pipeline is created. So commit that file to `main` first:

   ```bash
   go run ./e2e -dry-run -providers azure -out /tmp/e2e
   # commit /tmp/e2e/azure/azure-pipelines.yml to the repository's main branch
   ```

   Then either let the first run create the pipeline, or create it yourself
   under **Pipelines › New pipeline › Azure Repos Git › Existing YAML file**
   (save it without running, and name it `ci-shift-e2e`).
4. Create an **organization-scoped** personal access token (**User settings
   › Personal access tokens**). Global PATs stop working on 2026-12-01. Give
   it the scopes **Code: Read & write** and **Build: Read & execute**.
5. Add to the `e2e` environment:

| Kind | Name | Value |
|---|---|---|
| secret | `E2E_AZURE_TOKEN` | the PAT |
| variable | `E2E_AZURE_ORG` | organization name |
| variable | `E2E_AZURE_PROJECT` | project name |
| variable | `E2E_AZURE_REPO` | repository name |
| variable | `E2E_AZURE_PIPELINE` | optional; pipeline name, default `ci-shift-e2e` |

The runner stores `E2E_SECRET` as a secret variable on the pipeline
definition. The converted YAML maps it into the step's `env`, because Azure
does not expose secrets on its own. The push to `e2e/*` triggers the run.
If no build appears within two minutes (for example because implied YAML
triggers are turned off), the runner queues one for the branch.

### CircleCI

CircleCI only builds repositories hosted on GitHub or Bitbucket, so the
target is a second, empty GitHub repository that CircleCI follows.

1. Create a private GitHub repository, e.g. `viniciusdc/ci-shift-e2e-circleci`,
   with an initial commit.
2. Sign up at [circleci.com](https://circleci.com) with GitHub and **create
   a project** for that repository. New projects use the GitHub App
   integration, whose default trigger runs on every push. If the project
   slug (**Project settings › Overview**) is `circleci/<org-id>/<project-id>`
   rather than `gh/<owner>/<repo>`, set `E2E_CIRCLECI_SLUG` to it.
3. Create a CircleCI **personal API token** (**User settings › Personal API
   tokens**). CircleCI tokens have no scopes, so this is another reason to
   use a dedicated account.
4. Create a GitHub **fine-grained personal access token** that can access
   only the throwaway repository, with **Contents: Read and write** (GitHub
   adds Metadata: Read). This token pushes the test branches.
5. Add to the `e2e` environment:

| Kind | Name | Value |
|---|---|---|
| secret | `E2E_CIRCLECI_TOKEN` | the CircleCI token |
| secret | `E2E_CIRCLECI_GH_TOKEN` | the GitHub token |
| variable | `E2E_CIRCLECI_REPO` | `<owner>/<repo>` on GitHub |
| variable | `E2E_CIRCLECI_SLUG` | optional; the project slug, default `gh/<owner>/<repo>` |

The runner replaces the project environment variable `E2E_SECRET`. CircleCI
does not start a pipeline for a branch created without a new commit. Every
run pushes a new commit, so this does not come up.

## Cost

One run is a few minutes of each provider's free allowance, plus the GitHub
Actions minutes of the jobs that wait for it, typically 5–15 minutes per
provider. On private repositories those GitHub minutes count against the
account's quota. The estimates below are rough.

| Provider | Free allowance | One run |
|---|---|---|
| GitLab | 400 compute minutes a month | ~10 jobs × ~1 min |
| Bitbucket | 50 build minutes a month, per workspace | ~9 steps × ~1 min, the scarcest budget |
| Azure DevOps | 1,800 minutes a month with the free grant | ~10 jobs × ~1 min |
| CircleCI | 30,000 credits a month | ~10 jobs × ~1 min on a medium Docker executor (~10 credits a minute) |

## Layout

| Path | |
|---|---|
| [`conformance/`](conformance) | `ci.yml`, the expected outcome of each job, and the offline conversion test |
| [`internal/runner/`](internal/runner) | the `Provider` interface, one REST client per provider, git push, polling and the summary |
| [`main.go`](main.go) | the `go run ./e2e` command; not part of the `cishift` binary |
| [`../.github/workflows/e2e.yaml`](../.github/workflows/e2e.yaml) | the manual workflow |
