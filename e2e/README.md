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

Every job runs in `buildpack-deps:bookworm-scm`, Debian with git, ssh and
CA certificates. CircleCI's `checkout` needs those in the job image, and the
image meets Azure's container-job requirements (bash, glibc, no
`ENTRYPOINT`). The
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

About 15 minutes.

1. Create a GitLab.com account. A dedicated account keeps the token's reach
   small. Sign-up always verifies the email; depending on a risk score GitLab
   may also ask for a phone number or a credit card (not charged).
2. **Create new (+) › New project/repository › Create blank project**: name
   it `ci-shift-e2e`, make it private, keep **Initialize repository with a
   README**, and put it in your personal namespace.
3. Instance runners are on by default on GitLab.com; **Settings › CI/CD ›
   Runners › Turn on instance runners for this project** if not. Run one
   pipeline by hand (**Build › Pipelines › New pipeline**) before the first
   e2e run: if it fails with *Identity verification is required in order to
   run CI jobs*, finish the verification from the banner. An unattended run
   would otherwise fail the same way.
4. Avatar › **Edit profile › Access › Personal access tokens › Generate
   token › Legacy token**, scope **`api`**. It covers the git push too, so
   `write_repository` is not needed; `read_api` is not enough, because it
   cannot write CI/CD variables. Expiry is at most 365 days, and GitLab
   emails you 60, 30 and 7 days before. As the project's creator you are its
   Owner, which covers the Maintainer role that variables require.
   Project access tokens would be narrower but need Premium on GitLab.com.
5. Add to the `e2e` environment:

| Kind | Name | Value |
|---|---|---|
| secret | `E2E_GITLAB_TOKEN` | the token |
| variable | `E2E_GITLAB_PROJECT` | `<username>/ci-shift-e2e` |
| variable | `E2E_GITLAB_URL` | optional; a self-managed instance, default `https://gitlab.com` |

The runner creates or updates the project variable `E2E_SECRET`: masked,
and not protected, because protected variables are hidden from the
unprotected `e2e/*` branches.

### Bitbucket

About 15 minutes.

1. Sign up at [bitbucket.org](https://bitbucket.org) with an Atlassian
   account and name a workspace when asked. Consider a workspace just for
   these tests: the Free plan's 50 build minutes a month are shared by every
   repository in the workspace.
2. Create a private repository `ci-shift-e2e` with a README, so it has a
   main branch.
3. Turn on Pipelines: **Repository settings › Pipelines › Settings › Enable
   Pipelines**, or run the wizard once from **Pipelines › Create your first
   pipeline**.
4. Repository **… › Settings › Security › Access tokens › Create access
   token** (every plan has them). Tick **Repositories: Read** and **Write**,
   **Pipelines: Read** and **Pipelines: Edit variables** (`repository`,
   `repository:write`, `pipeline`, `pipeline:variable`). The token is shown
   once. git authenticates as `x-token-auth`, the REST API with a Bearer
   header; the runner handles both.
   - Alternatively, an Atlassian API token (**Account settings › Security ›
     Create API token with scopes**, app **Bitbucket**) with
     `read:repository:bitbucket`, `write:repository:bitbucket`,
     `read:pipeline:bitbucket` and `admin:pipeline:bitbucket`. Set
     `E2E_BITBUCKET_EMAIL` with it: the runner then authenticates git as
     `x-bitbucket-api-token-auth`. App passwords stopped working on
     2026-07-28.
5. Add to the `e2e` environment:

| Kind | Name | Value |
|---|---|---|
| secret | `E2E_BITBUCKET_TOKEN` | the token |
| variable | `E2E_BITBUCKET_WORKSPACE` | workspace ID |
| variable | `E2E_BITBUCKET_REPO` | repository slug |
| variable | `E2E_BITBUCKET_EMAIL` | only with an API token: your Atlassian account email |

The runner creates or updates the secured repository variable `E2E_SECRET`.
When the workspace runs out of build minutes, Bitbucket pauses the pipeline
instead of failing it, and Free plans cannot buy more; the runner reports a
paused pipeline as failed.

### Azure DevOps

About 30 minutes, plus waiting for the free build capacity.

1. At [dev.azure.com](https://dev.azure.com), create an organization and a
   **private** project (new public projects are no longer allowed), then in
   **Repos** initialize the repository with a README on `main`.
2. Get the free Microsoft-hosted parallel job (1 job, 1,800 minutes a month;
   private projects have none until then). **Organization settings › Billing
   › Set up billing** and link an Azure subscription in the same Microsoft
   Entra directory. It must be a paid (pay-as-you-go) subscription: the Azure
   free trial is not accepted. The free job itself costs nothing. Check
   **Organization settings › Pipelines › Parallel jobs**: it should show 1
   Microsoft-hosted job. If it still shows 0, submit
   https://aka.ms/azpipelines-parallelism-request, which Microsoft answers in
   about 4–5 business days.
3. Commit the converted pipeline to `main`, since Azure reads it when the
   pipeline is created:

   ```bash
   go run ./e2e -dry-run -providers azure -out /tmp/e2e
   # commit /tmp/e2e/azure/azure-pipelines.yml to the repository's main branch
   ```

   Then let the first run create the pipeline, or create it yourself under
   **Pipelines › New pipeline › Azure Repos Git › Existing Azure Pipelines
   YAML file**, **Save** without running, and rename it `ci-shift-e2e`.
4. **Organization settings › Pipelines › Settings**: leave **Disable implied
   YAML CI trigger** off (the default), or pushes to `e2e/*` start nothing.
   The runner queues a build itself after two minutes, but the trigger is
   faster.
5. **User settings › Personal access tokens › New Token**, scoped to this
   organization (global PATs stop working on 2026-12-01), with **Code: Read &
   write** and **Build: Read & execute**. Those cover the push, creating the
   pipeline, its secret variable, queueing builds and reading their results.
6. Add to the `e2e` environment:

| Kind | Name | Value |
|---|---|---|
| secret | `E2E_AZURE_TOKEN` | the PAT |
| variable | `E2E_AZURE_ORG` | organization name |
| variable | `E2E_AZURE_PROJECT` | project name |
| variable | `E2E_AZURE_REPO` | repository name |
| variable | `E2E_AZURE_PIPELINE` | optional; pipeline name, default `ci-shift-e2e` |

The runner stores `E2E_SECRET` as a secret variable on the pipeline
definition. The converted YAML maps it into the step's `env`, because Azure
does not expose secrets on its own. With a single free parallel job, runs
queue behind any other build in the organization.

### CircleCI

About 20 minutes. CircleCI only builds repositories hosted on GitHub or
Bitbucket, so the target is a second GitHub repository that CircleCI
follows.

1. Create a private GitHub repository, e.g.
   `viniciusdc/ci-shift-e2e-circleci`, with an initial commit.
2. Sign up at [circleci.com](https://circleci.com/signup/) with GitHub, then
   **Create Project › Set up a pipeline › Choose a repo › GitHub Cloud**, and
   install the CircleCI GitHub App on that repository only.
3. **Project Settings › Project Setup**: make sure the trigger is **All
   pushes**. A default-branch or pull-request trigger never runs the `e2e/*`
   branches.
4. Copy the project slug from **Project Settings**. GitHub App projects look
   like `circleci/<org-id>/<project-id>`, not `gh/<owner>/<repo>`, so the
   runner does not guess it.
5. **User settings › Personal API Tokens › Create New Token**. These tokens
   have no scopes and expire within a year; project tokens do not work with
   the API the runner uses.
6. On GitHub, **Settings › Developer settings › Fine-grained tokens ›
   Generate new token**: only the throwaway repository, **Contents: Read and
   write** (GitHub adds Metadata: Read). This token pushes and deletes the
   test branches.
7. Add to the `e2e` environment:

| Kind | Name | Value |
|---|---|---|
| secret | `E2E_CIRCLECI_TOKEN` | the CircleCI token |
| secret | `E2E_CIRCLECI_GH_TOKEN` | the GitHub token |
| variable | `E2E_CIRCLECI_REPO` | `<owner>/<repo>` on GitHub |
| variable | `E2E_CIRCLECI_SLUG` | the project slug from step 4 |

The runner deletes and recreates the project environment variable
`E2E_SECRET` (CircleCI cannot update one in place). CircleCI starts no
pipeline for a pushed branch without a new commit; every run pushes a new
commit, so this does not come up.

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
