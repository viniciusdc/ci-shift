# CI Schema Validator with CLI

## Overview
This Go tool validates CI workflow YAML files (GitHub Actions, CircleCI, GitLab CI) against their respective JSON schemas from SchemaStore and GitLab, with a **CLI interface for local validation and CI pipelines**.

It:
- Downloads and caches JSON schemas locally in `~/.cache/ci-schema-cache`.
- Loads YAML workflow files, converts to JSON, and validates against the schema.
- Supports validation for GitHub Actions, CircleCI, and GitLab CI.
- Provides a CLI to validate files directly from the terminal.

## Structure
- `schema/loader.go`: Schema download and caching logic.
- `schemas/constants.go`: Schema URL constants.
- `validator/validate.go`: Core validation API.
- `cmd/root.go`: Cobra CLI entry point.
- `main.go`: CLI application launcher.

## CLI Usage
### Installation
```bash
go build -o civalidator .
```

### Validate a Workflow File
```bash
./civalidator validate --provider github .github/workflows/main.yml
```

### Flags
- `--provider` (required): `github`, `circleci`, `gitlab`.

## Programmatic Usage
```go
errs, err := validator.ValidateWorkflowForProvider("path/to/workflow.yaml", validator.GitHubActions)
if err != nil {
    log.Fatal(err)
}
if len(errs) > 0 {
    for _, e := range errs {
        fmt.Println("Validation error:", e)
    }
} else {
    fmt.Println("Workflow is valid!")
}
```

## License
MIT

---

Let me know if you want me to generate the **full `main.go` and `cmd/root.go` CLI implementation with Cobra** now to complete your CLI workflow.
