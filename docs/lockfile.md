# Keeping generated files in sync

Writing a file with `-o` records how it was produced in `.cishift.lock`.
cishift uses the nearest `.cishift.lock` in the current directory or its
parents, or creates one at the root of the git repository (or in the current
directory outside a repository). Commit it. The generated pipelines stay
untouched; all the provenance lives in the lockfile:

```yaml
version: 1
generated:
  - output: .github/workflows/ci.yml
    source: .gitlab-ci.yml
    from: gitlab
    to: github
    cishift: v0.1.0
    schema: github@sha256:d10c9f4656e1
    source_sha256: sha256:e786…
    output_sha256: sha256:f672…
```

Paths in the lockfile are relative to the lockfile's directory. `schema`
records which schema snapshot was in use, and `cishift` the version that
wrote the file.

## Verify and sync

```bash
cishift verify   # offline check of every recorded file; exits 1 on problems
cishift sync     # regenerate them all (add --force to overwrite hand edits)
```

`verify` reports four kinds of problem:

- an output is missing;
- an output was **edited by hand** since it was generated;
- its **source changed**;
- the installed cishift would now **generate it differently**, e.g. after an
  upgrade.

## Hand edits

Regenerating a file that was edited by hand is refused, both by a plain
conversion with `-o` and by `sync`, unless you pass `--force`. Use
`--no-lock` to write a file without recording it, and `--lock <path>` to use
a specific lockfile instead of the nearest one.

The [README](../README.md#keeping-generated-files-in-sync) shows the whole
round trip; it is [testdata/examples/lockfile.txtar](../testdata/examples/lockfile.txtar),
which runs in CI.
