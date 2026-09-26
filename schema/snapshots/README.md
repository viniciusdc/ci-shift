# Schema snapshots

Each provider's schema, as published upstream, built into cishift so
`cishift check` works offline. The files are stored unmodified and
refreshed with `make schemas`.

| File | Source | License |
|---|---|---|
| `github.json` | [SchemaStore](https://github.com/SchemaStore/schemastore) | Apache-2.0 (see [LICENSE](../../LICENSE), [NOTICE](../../NOTICE)) |
| `circleci.json` | [circleci-yaml-language-server](https://github.com/CircleCI-Public/circleci-yaml-language-server) | Apache-2.0 (see [LICENSE](../../LICENSE)) |
| `gitlab.json` | [gitlab-org/gitlab](https://gitlab.com/gitlab-org/gitlab) | MIT ([licenses/gitlab.txt](licenses/gitlab.txt)) |
| `azure.json` | [azure-pipelines-vscode](https://github.com/microsoft/azure-pipelines-vscode) | MIT ([licenses/azure.txt](licenses/azure.txt)) |
| `bitbucket.json` | [Atlassian](https://api.bitbucket.org/schemas/pipelines-configuration) | none stated |
