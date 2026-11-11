# Contributing to kubectl-gpu-fit

Thanks for taking the time. Bug reports from real clusters, corrections to GPU and MIG data, and code are all welcome.

By participating you agree to follow the [Code of Conduct](CODE_OF_CONDUCT.md). Please report security problems privately as described in [SECURITY.md](SECURITY.md), not in public issues.

## Reporting a problem

The most useful bug report contains:

- the command you ran and its output (`-o wide` or `--explain` shows the reasoning),
- `kubectl gpu-fit version`,
- your dcgm-exporter or GPU Operator version, and
- the label set of one DCGM series, which is usually all it takes to reproduce attribution problems:

  ```sh
  curl -s "$PROM/api/v1/query?query=DCGM_FI_DEV_GPU_UTIL" | jq '.data.result[0].metric'
  ```

Redact pod and namespace names if you need to; the label *names* are what matter.

## Development setup

You need Go 1.26 or newer and, for linting, [golangci-lint](https://golangci-lint.run/) v2. No cluster and no GPU are required.

```sh
git clone https://github.com/cb-Sigma/kubectl-gpu-fit.git
cd kubectl-gpu-fit
make build        # bin/kubectl-gpu-fit
make check        # go vet, golangci-lint, go test -race: what CI runs
make demo         # the report from bundled sample data
```

### Trying changes end to end without a cluster

`hack/fake-cluster` serves an imitation Kubernetes API server with a fake Prometheus behind its service proxy, and writes a kubeconfig for it:

```sh
make fake-cluster                                   # terminal 1, keeps running
bin/kubectl-gpu-fit --kubeconfig fake-cluster.kubeconfig -A   # terminal 2
```

That exercises the real binary, real kubeconfig loading, the client-go REST client and Prometheus discovery through the service proxy. The data is invented (see `internal/testcluster`).

## Project layout

| Path | What lives there |
| --- | --- |
| `main.go` | Entry point; the binary must be named `kubectl-gpu-fit` to work as a kubectl plugin. |
| `internal/cmd` | Flags, wiring, end-to-end tests of the command. |
| `internal/kube` | Lists GPU pods and resolves their owners. Read-only. |
| `internal/prom` | Minimal Prometheus query client, service-proxy transport, discovery. |
| `internal/dcgm` | The PromQL queries and the mapping from DCGM labels to pods. |
| `internal/gpu` | GPU model catalog and MIG profile tables. |
| `internal/pricing` | Default price table, `--price` and `--price-file`. |
| `internal/recommend` | The recommendation engine. Pure functions, no I/O. |
| `internal/report` | Joins everything into rows and renders them. |
| `internal/demo` | Sample data behind `--demo`. |
| `internal/promtest`, `internal/kubetest`, `internal/testcluster` | Fakes shared by tests and `hack/fake-cluster`. |
| `deploy/rbac.yaml` | Minimum RBAC. Update it whenever the tool needs a new permission. |

## Guidelines

- **Stay read-only.** The tool must never send anything but `get` and `list` to the API server; tests assert this. A change that needs a write verb will not be merged.
- **Stay conservative.** A recommendation must never be made unless peak memory plus headroom fits the target. `TestNeverRecommendsWhatDoesNotFit` checks that with random inputs; extend it when you add a strategy.
- **Cite sources for data.** Changes to MIG profiles, GPU memory sizes, metric or label names need a link to NVIDIA documentation or source in the pull request. `TestMIGTable` pins the MIG tables to the MIG User Guide.
- **Prices are estimates.** Keep the defaults rough and round; precise per-provider pricing belongs in a user's `--price-file`.
- **Test what you change.** New behaviour needs a test. The engine is table-driven; the command has end-to-end tests against fakes in `internal/cmd`.
- **Keep the README honest.** Command output in `README.md` is checked by `TestReadmeOutput`; refresh it with `make readme` rather than editing it by hand.

## Pull requests

1. Open an issue first for anything larger than a small fix, so the approach can be agreed before you invest time.
2. Create a branch, make focused commits, and run `make check`.
3. Open the pull request against `main` and fill in the template. CI must be green.

Commit messages: a short imperative subject (`Add H200 NVL MIG profiles`), and a body explaining why when it is not obvious.

## Releasing (maintainers)

Tag `vX.Y.Z` on `main` and push the tag. The release workflow runs GoReleaser, which builds the archives and publishes the GitHub release. `make snapshot` produces the same archives locally without publishing.

## License

By contributing you agree that your contributions are licensed under the [Apache License 2.0](LICENSE).
