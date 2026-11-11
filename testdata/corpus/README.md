# Pipeline corpus

CI/CD files from public projects, used by `TestCorpus` in
[`convert/corpus_test.go`](../../convert/corpus_test.go) to see how ci-shift
handles pipelines nobody wrote for it. Each file is stored unmodified, from
the commit in its link, under its provider's directory.

[`results.txt`](results.txt) records, for each file, the detected provider,
whether the file passes its own provider's schema, and the outcome of
converting it to every other provider: the job and warning counts, or the
error, and any schema issue in the output. The test fails when a result
changes; run `make golden` once the change is intended and review the diff.
Failures and `invalid` entries are known problems still to fix.

Only projects under permissive licenses are included. The license or
notice of each one is in [`licenses/`](licenses); the Apache License 2.0
text is in the repository's [`LICENSE`](../../LICENSE).

| File | Source | License |
|---|---|---|
| `github/cli.yml` | [cli/cli](https://github.com/cli/cli/blob/18374541e44ad568208c2f45b14cc51c201121bd/.github/workflows/go.yml) | MIT ([license](licenses/cli-cli.txt)) |
| `github/flask.yml` | [pallets/flask](https://github.com/pallets/flask/blob/49943b5a94413a530761350f04e896f76e8d620e/.github/workflows/tests.yaml) | BSD-3-Clause ([license](licenses/pallets-flask.txt)) |
| `github/prometheus.yml` | [prometheus/prometheus](https://github.com/prometheus/prometheus/blob/997549d4479dbc52382c83679dd998cd1c8eff27/.github/workflows/ci.yml) | Apache-2.0 ([notice](licenses/prometheus-prometheus.txt)) |
| `github/pytest.yml` | [pytest-dev/pytest](https://github.com/pytest-dev/pytest/blob/1b6f4419c6fe9371a8d8c52c12ed0a1577c2504a/.github/workflows/test.yml) | MIT ([license](licenses/pytest-dev-pytest.txt)) |
| `github/requests.yml` | [psf/requests](https://github.com/psf/requests/blob/3c7baaa05ab4aa49cc819ce3a819d260de19c157/.github/workflows/run-tests.yml) | Apache-2.0 ([notice](licenses/psf-requests.txt)) |
| `github/ruff.yml` | [astral-sh/ruff](https://github.com/astral-sh/ruff/blob/b5d383d9c781fcd96ddb7e82ded2790de93d8633/.github/workflows/ci.yaml) | MIT ([license](licenses/astral-sh-ruff.txt)) |
| `gitlab/gitaly.yml` | [gitlab-org/gitaly](https://gitlab.com/gitlab-org/gitaly/-/blob/5680c8451ac24bdd8234073f3eb91ec5aed1e8c4/.gitlab-ci.yml) | MIT ([license](licenses/gitlab-org-gitaly.txt)) |
| `gitlab/gitlab-development-kit.yml` | [gitlab-org/gitlab-development-kit](https://gitlab.com/gitlab-org/gitlab-development-kit/-/blob/5eb1712a46e26370ebdbdd1cdda4187a0817d363/.gitlab-ci.yml) | MIT ([license](licenses/gitlab-org-gitlab-development-kit.txt)) |
| `gitlab/gitlab-pages.yml` | [gitlab-org/gitlab-pages](https://gitlab.com/gitlab-org/gitlab-pages/-/blob/b5e79e3d3fd1c6cb78bc80a165a71ee11355b08d/.gitlab-ci.yml) | MIT ([license](licenses/gitlab-org-gitlab-pages.txt)) |
| `gitlab/gitlab-runner.yml` | [gitlab-org/gitlab-runner](https://gitlab.com/gitlab-org/gitlab-runner/-/blob/2e73aab778d8053c65e8ff56ea3272f7f1fef2c9/.gitlab-ci.yml) | MIT ([license](licenses/gitlab-org-gitlab-runner.txt)) |
| `gitlab/glab.yml` | [gitlab-org/cli](https://gitlab.com/gitlab-org/cli/-/blob/779ccb6f50d1f99b0f0728ca0ff27425cbd61027/.gitlab-ci.yml) | MIT ([license](licenses/gitlab-org-cli.txt)) |
| `circleci/aws-ecr-orb.yml` | [CircleCI-Public/aws-ecr-orb](https://github.com/CircleCI-Public/aws-ecr-orb/blob/0034d700e037a647215104ed71216bbb590a30d0/.circleci/test-deploy.yml) | MIT ([license](licenses/CircleCI-Public-aws-ecr-orb.txt)) |
| `circleci/circleci-cli.yml` | [CircleCI-Public/circleci-cli](https://github.com/CircleCI-Public/circleci-cli/blob/75e476473fad4133cfea6e53988ad2ccb9cd37c0/.circleci/config.yml) | MIT ([license](licenses/CircleCI-Public-circleci-cli.txt)) |
| `azure/azure-cli.yml` | [Azure/azure-cli](https://github.com/Azure/azure-cli/blob/74f7e490691019bd24dd46ca1e5712c57ea66af0/azure-pipelines.yml) | MIT ([license](licenses/Azure-azure-cli.txt)) |
| `azure/azure-sdk-for-python.yml` | [Azure/azure-sdk-for-python](https://github.com/Azure/azure-sdk-for-python/blob/dadfa96d3e1d714a05e31fb4a43485b98e57b339/eng/pipelines/templates/stages/archetype-sdk-client.yml) | MIT ([license](licenses/Azure-azure-sdk-for-python.txt)) |
| `azure/numpy-feedstock.yml` | [conda-forge/numpy-feedstock](https://github.com/conda-forge/numpy-feedstock/blob/47e682522bf73a8a5fa4bfcfb0b66feeb9eb583c/azure-pipelines.yml) | BSD-3-Clause ([license](licenses/conda-forge-numpy-feedstock.txt)) |
| `azure/terminal.yml` | [microsoft/terminal](https://github.com/microsoft/terminal/blob/5f7dc283013152f89bfb76950515c33f45157ad7/build/pipelines/ci.yml) | MIT ([license](licenses/microsoft-terminal.txt)) |
| `bitbucket/aws-ecr-push-image.yml` | [atlassian/aws-ecr-push-image](https://bitbucket.org/atlassian/aws-ecr-push-image/src/46324d98322ef8dc0f90da8f8a974faf4652bf9d/bitbucket-pipelines.yml) | Apache-2.0 ([notice](licenses/atlassian-aws-ecr-push-image.txt)) |
| `bitbucket/aws-s3-deploy.yml` | [atlassian/aws-s3-deploy](https://bitbucket.org/atlassian/aws-s3-deploy/src/1bdbb7dbe014019eaf03b00c6f14a40768549c04/bitbucket-pipelines.yml) | Apache-2.0 ([notice](licenses/atlassian-aws-s3-deploy.txt)) |
| `bitbucket/bitbucket-upload-file.yml` | [atlassian/bitbucket-upload-file](https://bitbucket.org/atlassian/bitbucket-upload-file/src/e148ce573c38d074b5fc1a6e28544f95fc14d2e8/bitbucket-pipelines.yml) | Apache-2.0 ([notice](licenses/atlassian-bitbucket-upload-file.txt)) |
| `bitbucket/slack-notify.yml` | [atlassian/slack-notify](https://bitbucket.org/atlassian/slack-notify/src/2f773f3986e4c406d078fc754fd64b09a4b7db65/bitbucket-pipelines.yml) | Apache-2.0 ([notice](licenses/atlassian-slack-notify.txt)) |
| `bitbucket/ssh-run.yml` | [atlassian/ssh-run](https://bitbucket.org/atlassian/ssh-run/src/5c36a033a8ae2bdf53551e08e7d199c8eb53582b/bitbucket-pipelines.yml) | Apache-2.0 ([notice](licenses/atlassian-ssh-run.txt)) |
