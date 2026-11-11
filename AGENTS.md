# AGENTS.md

Notes for coding agents working on ci-shift. The sections between
`<!-- gendocs:... -->` markers are generated from the code by
`internal/tools/gendocs`; run `make docs` instead of editing them.

## What ci-shift is

`cishift` is a Go command-line tool that converts CI/CD pipeline files
between GitHub Actions, GitLab CI/CD, CircleCI, Azure Pipelines and
Bitbucket Pipelines, and checks pipeline files against each provider's JSON
schema. What it cannot translate is reported as a warning and listed in a
review header at the top of the output.

## How a conversion works

1. `convert.Detect` picks the source provider from the file's path and
   content, unless `--from` names it.
2. The source provider's `Parse` (`providers/<name>/parse.go`) reads the
   file into an `ir.Pipeline`, the provider-neutral model in `ir/`.
3. `Pipeline.Validate` checks the model: unique job IDs, known
   dependencies, no cycles.
4. The target provider's `Emit` (`providers/<name>/emit.go`) renders the
   model as the target's YAML. Canonical references such as
   `${{ ci.<name> }}` in text fields become the target's syntax (see the
   `ir` package comment).
5. Parse and Emit record what they drop or approximate in `ir.Diagnostics`:
   `Warnf` for lost behavior, `Infof` for notes. `convert.Convert` lists
   the warnings in the review header, and `--strict` turns them into exit
   code 1.
6. With `--check`, `schema/` validates the output against the target's
   schema: the snapshot built into the binary, or the copy that
   `cishift schemas update` downloaded. With `-o`, `internal/lockfile`
   records the output in `.cishift.lock`, which `cishift verify` and
   `cishift sync` read.

The cobra commands are in `internal/cli`; `cmd/cishift` is the entry point.

## Where to make changes

- **A provider**: implement `ir.Provider` (`Name`, `Title`, `DefaultPath`,
  `Detect`, `Parse`, `Emit`) in `providers/<name>`, register it and its
  aliases in `convert/convert.go`, add its schema URL to `Sources` in
  `schema/schema.go`, run `make schemas` to snapshot it, and add a fixture
  under `testdata/<name>/`.
- **A feature that should translate**: add it to the model in `ir/ir.go` if
  it has no field yet, then to each provider's parser and emitter. Where a
  target has no equivalent, emit the closest form, or a failing placeholder
  step, and record a warning instead of returning an error. Update
  `docs/translation.md` when the list of translated concepts changes.
- **Schema snapshots**: `make schemas` refreshes `schema/snapshots/` from
  the URLs in `schema/schema.go`.
- **Commands and flags**: `internal/cli`, then `make docs` to regenerate
  `docs/CLI.md` and the command table below.

## Testing

- `go test ./...` (`make test`) needs no network. CI runs it with `-race`.
- Golden tests (`convert/convert_test.go`) convert every file in
  `testdata/<provider>/` to every provider. Each output must pass the
  target's schema, match `testdata/golden/<provider>_<name>.<target>.yml`,
  be detected as the target, and come out unchanged when parsed and
  emitted again. After an intended change to an output, run `make golden`
  and review the diff.
- The corpus test (`convert/corpus_test.go`) runs real pipelines from
  public projects, in `testdata/corpus/<provider>/`, through detection,
  `check` and a conversion to every other provider. It records each
  outcome, failures included, in `testdata/corpus/results.txt` and fails
  when one changes. A fix that changes an outcome updates the file with
  `make golden`. New corpus files must be under a permissive license,
  unmodified, pinned to a commit and listed in `testdata/corpus/README.md`
  with their license in `testdata/corpus/licenses/`.
- Provider tests (`providers/<name>/*_test.go`) cover parsing and emitting
  details.
- README examples are the testscripts in `testdata/examples/*.txtar`, run
  against the built binary by `cmd/cishift/script_test.go`. `make
  examples` renders them into the README, so change an example in its
  script, not in the README.
- Live tests: `e2e/` converts `e2e/conformance/ci.yml`, a GitHub Actions
  workflow with one job per translated feature, and runs it on the real
  providers. `go run ./e2e -dry-run` converts and validates it without
  network access; real runs start from the E2E workflow on GitHub (see
  `e2e/README.md`). A job added to `ci.yml` also goes into `Jobs` in
  `e2e/conformance/conformance.go`.

## Rules

- Do not edit generated content by hand: the sections between
  `<!-- gendocs:... -->` markers, `docs/CLI.md`, `docs/assets/`,
  `testdata/golden/` and `testdata/corpus/results.txt`. Regenerate it with
  `make docs`, `make generate` or `make golden`.
- `main` only takes pull requests, and they need the `test`, `lint` and
  `Generated docs are up to date` checks. The last one reruns the
  generators and fails on any difference, so commit what they produce.
- A README section shows either generated example images or shell code
  blocks, not both.
- Run `make fmt` and `make lint` before pushing.

## Commands

<!-- gendocs:commands -->
```sh
cishift <file|-> --to <provider> [flags]
```

Flags: `--check` `--force` `-f/--from FROM` `--lock LOCK` `--no-header` `--no-lock` `-o/--output OUTPUT` `-q/--quiet` `--strict` `-t/--to TO`

| Command | Description |
|---------|-------------|
| [`cishift check`](docs/CLI.md#cishift-check) | Check pipeline files against their provider's JSON schema |
| [`cishift detect`](docs/CLI.md#cishift-detect) | Print the provider a pipeline file belongs to |
| [`cishift providers`](docs/CLI.md#cishift-providers) | List supported providers |
| [`cishift schemas`](docs/CLI.md#cishift-schemas) | Show or update the JSON schemas used by 'cishift check' |
| [`cishift schemas reset`](docs/CLI.md#cishift-schemas-reset) | Remove downloaded schemas and use the built-in snapshots |
| [`cishift schemas status`](docs/CLI.md#cishift-schemas-status) | Show which schema each provider uses |
| [`cishift schemas update`](docs/CLI.md#cishift-schemas-update) | Download the current schemas into the cache |
| [`cishift sync`](docs/CLI.md#cishift-sync) | Regenerate every pipeline file recorded in the lockfile |
| [`cishift verify`](docs/CLI.md#cishift-verify) | Check that generated pipeline files are up to date and untouched |
| [`cishift version`](docs/CLI.md#cishift-version) | Print the cishift version |

Every flag and example: [docs/CLI.md](docs/CLI.md).
<!-- /gendocs:commands -->

## Make targets

<!-- gendocs:make -->
| Target | Description |
|--------|-------------|
| `make all` | The default target: build. |
| `make build` | Build ./cishift, stamped with the version from git describe. |
| `make clean` | Remove the built binary. |
| `make fmt` | Format the Go code. |
| `make lint` | Run golangci-lint (make install-tools installs it). |
| `make test` | Run the Go tests, including the golden, corpus and README example tests. |
| `make golden` | Regenerate the golden outputs and the corpus results after an intentional change. |
| `make install` | Build and copy cishift to INSTALL_DIR, ~/.local/bin by default. |
| `make uninstall` | Remove cishift from INSTALL_DIR. |
| `make install-tools` | Install golangci-lint, and freeze, which renders the README examples. |
| `make schemas` | Refresh the embedded schema snapshots from their upstream sources. |
| `make ci` | Lint, test and build. |
| `make docs` | The CLI reference in docs/CLI.md, and the layout, command and make target sections of README.md and AGENTS.md. |
| `make banner` | Render docs/assets/banner-{dark,light}.svg from docsmith.toml. |
| `make diagrams` | Render docs/assets/diagrams/*.svg from docs/diagrams/*.toml. |
| `make examples` | Run testdata/examples/*.txtar, then render them into the docs. |
| `make examples-check` | Fail when the rendered examples are out of date (used by CI). |
| `make generate` | Regenerate every generated file. |
<!-- /gendocs:make -->

## Project layout

<!-- gendocs:layout -->
```
AGENTS.md           Guide for coding agents
Makefile            Build, test, lint, schema and docs targets
cmd/                Executable entry points
  cishift/          cishift translates CI/CD pipelines between providers
convert/            convert wires the providers together
docs/               CLI reference, lockfile and translation guides
  diagrams/         Diagram sources (TOML) for gendiagram
docsmith.toml       Configuration for the docs generators
e2e/                Runs the conformance pipeline on real CI providers
  conformance/      conformance holds the pipeline the live tests run on every provider
  internal/         Packages private to the live tests
    runner/         runner pushes the conformance pipeline to real CI providers
go.mod              Module github.com/viniciusdc/ci-shift
internal/           Packages private to this module
  buildinfo/        buildinfo reports the version of the running cishift binary
  cli/              cli implements the cishift command line
  lockfile/         lockfile records which pipeline files cishift generated and from what
  tools/            Maintenance commands, run through make
    genbanner/      Generates the README banner SVGs from docsmith.toml
    gendiagram/     Generates architecture diagram SVGs from TOML definitions
    gendocs/        Generates the CLI reference, layout and other doc sections
    genexamples/    Generates executable documentation snippets
    updateschemas/  Refreshes the schema snapshots built into cishift
  yamlx/            yamlx reads and writes YAML documents, keeping the order of keys
ir/                 ir defines ci-shift's provider-neutral pipeline model
providers/          One parser + emitter per CI provider
  azure/            azure converts Azure Pipelines YAML definitions to and from the IR
  bitbucket/        bitbucket converts Bitbucket Pipelines configurations to and from the IR
  circleci/         circleci converts CircleCI 2.1 configurations to and from the IR
  github/           github converts GitHub Actions workflows to and from the IR
  gitlab/           gitlab converts GitLab CI/CD configurations to and from the IR
schema/             schema validates pipeline files against the providers' JSON schemas
  snapshots/        Provider JSON schemas built into the binary
```
<!-- /gendocs:layout -->
