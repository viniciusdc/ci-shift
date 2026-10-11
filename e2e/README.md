# Live provider tests

The unit and golden tests check that ci-shift produces valid YAML. These
tests check that the YAML does what the source says when a real provider
runs it. [`conformance/ci.yml`](conformance/ci.yml) is a GitHub Actions
workflow in which each job exercises one feature and fails if ci-shift
mistranslated it. The runner converts it to each provider, pushes the result
to a fresh branch of a throwaway repository there, waits for the run, checks
every job, and deletes the branch, even when something failed.

```
conformance/ci.yml ──cishift──▶ .gitlab-ci.yml ──git push──▶ e2e/<run>-gitlab ──API──▶ jobs ✔/✘
                             ├─▶ .circleci/config.yml   (GitHub repo followed by CircleCI)
                             ├─▶ azure-pipelines.yml    (Azure Repos)
                             └─▶ bitbucket-pipelines.yml
```

`go test ./...` already checks, offline, that the pipeline converts to all
four targets without a warning and passes each target's schema. The live run
reports a provider without credentials as **skipped**, and stays green.

## What the conformance pipeline checks

| Job | Passes when | Fails when ci-shift gets wrong… |
|---|---|---|
| `checkout-repo` | `e2e/conformance/ci.yml` is in the working directory | checkout |
| `artifact-produce` | it writes `handoff/token.txt` and publishes it | artifact upload |
| `artifact-consume` | it runs after `artifact-produce` and reads `made-by-produce` from the artifact | `needs`, artifact passing |
| `matrix-legs` | there are exactly two legs, each with a value from `red`/`blue` | matrix expansion, `${{ matrix.* }}` |
| `env-vars` | scripts stop at the first failing command (`errexit`, as GitHub's `bash -e`), pipeline-, job- and step-level variables have their values, and `E2E_SECRET` is non-empty (checked with `test -n`, never printed) | shell semantics, variable scoping, secret mapping |
| `postgres-service` | a `postgres:16` service accepts TCP connections on 5432 (`postgres`, `localhost` or `127.0.0.1`, whichever the provider uses) | service containers |
| `cache-roundtrip` | the cache restore and save steps run | cache steps |
| `on-test-branch` | it runs, because its condition matches the `e2e/` branch prefix | branch conditions |
| `never-runs` | it is skipped or not scheduled (it runs `exit 1`) | branch conditions |

Every job runs in `buildpack-deps:bookworm-scm`, Debian with git, ssh and CA
certificates, which CircleCI's `checkout` needs; it also meets Azure's
container-job requirements (bash, glibc, no `ENTRYPOINT`). The runner
matches jobs by name, ignoring case and punctuation, so `matrix-legs (red)`
and `matrix_legs red` both count as legs of `matrix-legs`. When you change
`ci.yml`, update `Jobs` in [`conformance/conformance.go`](conformance/conformance.go);
a test keeps the two in step.

## Running it

From GitHub: **Actions › E2E › Run workflow**. The `providers` input takes a
comma-separated list (default: all four). Each provider runs as its own job
in the `e2e` environment, and runs never overlap, since they share the
throwaway repositories. Locally:

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

The exit code is 1 when any provider that ran failed; skipped providers
never fail the run.

### Reading the results

The summary, on stdout and in the job's step summary, gives each provider's
result and run link, then each job's check with ✅ or ❌ and the reason.

| Result | Meaning |
|---|---|
| ✅ passed | every job behaved as expected and the run succeeded |
| ❌ failed | a check failed, the run failed or timed out, or setup/push failed (the reason says which) |
| ⏭️ skipped | settings are missing in the `e2e` environment; the row lists which |
| 📝 dry-run | `-dry-run`: converted and validated only |

## Setup

All settings live in a GitHub environment named `e2e` (**Settings ›
Environments**); each table below says which are secrets and which are
variables. Fork pull requests cannot read the environment, required
reviewers can approve each run, and each job only gets its own provider's
settings. You never set `E2E_SECRET`: each run generates one, stores it in
the provider through its API and masks it.

### GitLab

1. On GitLab.com, create a private blank project `ci-shift-e2e` with a
   README.
2. Run one pipeline by hand (**Build › Pipelines › New pipeline**) and
   complete identity verification if GitLab asks for it; unattended runs
   fail until you do.
3. Create a personal access token (legacy) with the `api` scope: **Edit
   profile › Access › Personal access tokens**.
4. Add to the `e2e` environment:

| Kind | Name | Value |
|---|---|---|
| secret | `E2E_GITLAB_TOKEN` | the token |
| variable | `E2E_GITLAB_PROJECT` | `<username>/ci-shift-e2e` |
| variable | `E2E_GITLAB_URL` | optional; a self-managed instance, default `https://gitlab.com` |

### Bitbucket

1. Create a workspace at [bitbucket.org](https://bitbucket.org) and a
   private repository `ci-shift-e2e` with a README.
2. Enable Pipelines: **Repository settings › Pipelines › Settings**.
3. Create a repository access token (**Repository settings › Security ›
   Access tokens**) with **Repositories: Read** and **Write**, **Pipelines:
   Read** and **Pipelines: Edit variables**. An Atlassian API token for
   Bitbucket also works, with `read:repository:bitbucket`,
   `write:repository:bitbucket`, `read:pipeline:bitbucket` and
   `admin:pipeline:bitbucket`, if you also set `E2E_BITBUCKET_EMAIL`.
4. Add to the `e2e` environment:

| Kind | Name | Value |
|---|---|---|
| secret | `E2E_BITBUCKET_TOKEN` | the token |
| variable | `E2E_BITBUCKET_WORKSPACE` | workspace ID |
| variable | `E2E_BITBUCKET_REPO` | repository slug |
| variable | `E2E_BITBUCKET_EMAIL` | only with an API token: your Atlassian account email |

### Azure DevOps

1. At [dev.azure.com](https://dev.azure.com), create an organization and a
   private project, and initialize its repository with a README on `main`.
2. Link a paid Azure subscription (**Organization settings › Billing**; the
   free trial is not accepted) to get the free Microsoft-hosted job.
3. Add a self-hosted agent pool `ci-shift-e2e` with access for all
   pipelines (**Organization settings › Agent pools**). The workflow joins
   it as a temporary agent for each run, and a run takes a few minutes. On
   the free hosted job alone, each job can wait minutes for an agent and a
   run can outlast the runner's 30-minute `-timeout`.
4. Commit the converted pipeline to `main`, where Azure reads it when the
   first run creates the pipeline:

   ```bash
   go run ./e2e -dry-run -providers azure -out /tmp/e2e
   # commit /tmp/e2e/azure/azure-pipelines.yml to the repository's main branch
   ```

5. Give the workflow an identity. With GitHub's OIDC token, no Azure
   credential is stored in GitHub:
   1. Connect the organization to a Microsoft Entra directory
      (**Organization settings › Microsoft Entra ID**).
   2. In an Azure subscription of that directory, create a user-assigned
      managed identity with a federated credential for this repository's
      `e2e` environment. The subject must match GitHub's token exactly, and
      some repositories use ID-based subjects, so read its prefix from the
      API:

      ```bash
      prefix=$(gh api repos/<owner>/<repo>/actions/oidc/customization/sub --jq .sub_claim_prefix)
      az identity create -g <resource-group> -n ci-shift-e2e-github
      az identity federated-credential create -g <resource-group> \
        --identity-name ci-shift-e2e-github -n github-e2e-environment \
        --issuer https://token.actions.githubusercontent.com \
        --subject "$prefix:environment:e2e" \
        --audiences api://AzureADTokenExchange
      ```

   3. **Organization settings › Users › Add users**: the identity, with
      **Basic** access and the project's **Contributors** group.
   4. If the pipeline already exists, allow **Edit build pipeline** for the
      identity in the pipeline's **Manage security**: the runner writes
      `E2E_SECRET` into it.
   5. With a pool, add the identity itself (not the *Build Service* account
      or a group) as **Administrator** in the pool's **Security**, so it can
      register the agent.
   6. Restrict the `e2e` environment to the default branch (**Settings ›
      Environments › e2e › Deployment branches**): the federated credential
      trusts every run that uses the environment.

   Without a directory, use a personal access token scoped to the
   organization (**User settings › Personal access tokens**) with **Code:
   Read & write**, **Build: Read & execute** and, with a pool, **Agent
   Pools: Read & manage**.
6. Add to the `e2e` environment:

| Kind | Name | Value |
|---|---|---|
| secret | `E2E_AZURE_CLIENT_ID` | with OIDC: the identity's client ID |
| secret | `E2E_AZURE_TENANT_ID` | with OIDC: the directory (tenant) ID |
| secret | `E2E_AZURE_TOKEN` | without OIDC: the PAT |
| secret | `E2E_AZURE_ORG` | organization name |
| variable | `E2E_AZURE_PROJECT` | project name |
| variable | `E2E_AZURE_REPO` | repository name |
| variable | `E2E_AZURE_POOL` | optional; the self-hosted pool from step 3 (`ci-shift-e2e`) |
| variable | `E2E_AZURE_PIPELINE` | optional; pipeline name, default `ci-shift-e2e` |

The client ID, tenant ID and organization are not credentials, but as
secrets they are masked in the workflow logs, which are public on a public
repository.

With `E2E_AZURE_POOL` set, the runner points every `pool:` of the converted
pipeline at that pool, the only change it makes to the conversion. To use
the pool from `go run ./e2e`, run an agent in it on your machine (Docker
required; see Microsoft's [Linux agent guide](https://learn.microsoft.com/en-us/azure/devops/pipelines/agents/linux-agent)).

### CircleCI

1. Create a private GitHub repository for CircleCI to follow, e.g.
   `<owner>/ci-shift-e2e-circleci`, with an initial commit.
2. Sign up at [circleci.com](https://circleci.com/signup/) with GitHub and
   create a project for the repository, installing the CircleCI GitHub App
   on that repository only.
3. Set the project's trigger to **All pushes** (**Project Settings › Project
   Setup**); other triggers never run the `e2e/*` branches.
4. Copy the project slug from **Project Settings**; GitHub App projects use
   `circleci/<org-id>/<project-id>`, which the runner cannot derive.
5. Create a CircleCI personal API token (**User settings › Personal API
   Tokens**), and a GitHub fine-grained token for the repository with
   **Contents: Read and write**, which pushes and deletes the test branches.
6. Add to the `e2e` environment:

| Kind | Name | Value |
|---|---|---|
| secret | `E2E_CIRCLECI_TOKEN` | the CircleCI token |
| secret | `E2E_CIRCLECI_GH_TOKEN` | the GitHub token |
| variable | `E2E_CIRCLECI_REPO` | `<owner>/<repo>` on GitHub |
| variable | `E2E_CIRCLECI_SLUG` | the project slug from step 4 |

## Cost

A run uses a few minutes of each provider's free allowance: 400 compute
minutes a month on GitLab, 50 build minutes a month per Bitbucket workspace,
30,000 credits a month on CircleCI and 1,800 Microsoft-hosted minutes a month
on Azure, while a self-hosted Azure pool has no minute limit. A Bitbucket
workspace out of minutes pauses the pipeline, which the runner reports as
failed. On private repositories, the GitHub Actions jobs that wait for the
providers also use the account's Actions minutes.
