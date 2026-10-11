# CLI reference

Generated from the `--help` output of `cishift` by
`internal/tools/gendocs`; run `make docs` to refresh it. Edits between the
markers are overwritten.

<!-- gendocs:cli -->
ci-shift translates CI/CD configurations between GitHub Actions, GitLab CI/CD,
CircleCI, Azure Pipelines and Bitbucket Pipelines through a shared pipeline
model, and checks them against each provider's JSON schema.

```sh
cishift <file|-> --to <provider> [flags]
```

| Flag | Default | Description |
|------|---------|-------------|
| `--check` |  | check the output against the target provider's schema |
| `--force` |  | overwrite an output that was edited by hand |
| `-f/--from FROM` |  | source provider (detected from the file when omitted) |
| `--lock LOCK` | `nearest .cishift.lock` | lockfile to record the output in |
| `--no-header` |  | omit the review comment header |
| `--no-lock` |  | do not record the output in the lockfile |
| `-o/--output OUTPUT` | `stdout` | output file |
| `-q/--quiet` |  | do not print diagnostics |
| `--strict` |  | exit 1 when the conversion produced warnings |
| `-t/--to TO` |  | target provider: github, gitlab, circleci, azure, bitbucket |

```sh
cishift .gitlab-ci.yml --to github -o .github/workflows/ci.yml
cishift .circleci/config.yml --to azure --check --strict
cat bitbucket-pipelines.yml | cishift - --from bitbucket --to gitlab
cishift check .github/workflows/*.yml
```

| Command | Description |
|---------|-------------|
| [`check`](#cishift-check) | Check pipeline files against their provider's JSON schema |
| [`detect`](#cishift-detect) | Print the provider a pipeline file belongs to |
| [`providers`](#cishift-providers) | List supported providers |
| [`schemas`](#cishift-schemas) | Show or update the JSON schemas used by 'cishift check' |
| [`sync`](#cishift-sync) | Regenerate every pipeline file recorded in the lockfile |
| [`verify`](#cishift-verify) | Check that generated pipeline files are up to date and untouched |
| [`version`](#cishift-version) | Print the cishift version |

---

### `cishift check`

Check pipeline files against their provider's JSON schema

```sh
cishift check <file|->... [--embedded] [--latest] [-p/--provider PROVIDER]
```

Aliases: `validate`

| Flag | Description |
|------|-------------|
| `--embedded` | use the schemas built into cishift, ignoring updated copies |
| `--latest` | download the latest schemas first (updates the cache) |
| `-p/--provider PROVIDER` | provider (detected from the file when omitted) |

---

### `cishift detect`

Print the provider a pipeline file belongs to

```sh
cishift detect <file|->...
```

---

### `cishift providers`

List supported providers

```sh
cishift providers
```

---

### `cishift schemas`

cishift ships a snapshot of every provider's schema, so checks work offline
and give the same result everywhere. 'cishift schemas update' downloads the
current schemas into your cache; from then on they are used instead of the
built-in ones. 'cishift schemas reset' goes back to the built-in snapshots.

```sh
cishift schemas
```

| Subcommand | Description |
|------------|-------------|
| [`reset`](#cishift-schemas-reset) | Remove downloaded schemas and use the built-in snapshots |
| [`status`](#cishift-schemas-status) | Show which schema each provider uses |
| [`update`](#cishift-schemas-update) | Download the current schemas into the cache |

#### `cishift schemas reset`

Remove downloaded schemas and use the built-in snapshots

```sh
cishift schemas reset
```

#### `cishift schemas status`

Show which schema each provider uses

```sh
cishift schemas status
```

#### `cishift schemas update`

Download the current schemas into the cache

```sh
cishift schemas update
```

---

### `cishift sync`

Regenerate every pipeline file recorded in the lockfile

```sh
cishift sync [--force] [--lock LOCK]
```

| Flag | Default | Description |
|------|---------|-------------|
| `--force` |  | overwrite outputs that were edited by hand |
| `--lock LOCK` | `nearest .cishift.lock` | lockfile to sync |

---

### `cishift verify`

verify checks every file recorded in the lockfile, offline, and fails when
an output is missing or was edited by hand, when its source changed since it
was generated, or when the installed cishift would now generate it
differently. Run 'cishift sync' to regenerate.

```sh
cishift verify [--lock LOCK]
```

| Flag | Default | Description |
|------|---------|-------------|
| `--lock LOCK` | `nearest .cishift.lock` | lockfile to verify |

---

### `cishift version`

Print the cishift version

```sh
cishift version [--check]
```

| Flag | Description |
|------|-------------|
| `--check` | check GitHub for a newer release |
<!-- /gendocs:cli -->
