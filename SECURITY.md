# Security Policy

## Supported versions

Security fixes are made on the latest release line. Please upgrade to the newest version before reporting.

## Reporting a vulnerability

Please do **not** open a public issue for security problems.

Report privately through GitHub: on the repository page, open **Security**, then **Report a vulnerability**, or go straight to <https://github.com/cb-Sigma/kubectl-gpu-fit/security/advisories/new>.

Include what you found, how to reproduce it, the version (`kubectl gpu-fit version`), and the impact you expect. You can expect an acknowledgement within a few working days. Once a fix is ready, a release and an advisory are published, with credit to you unless you prefer otherwise.

## Scope and design notes

`kubectl gpu-fit` is a client-side tool. It runs with the credentials of the person invoking it and is designed to be strictly read-only:

- Towards the Kubernetes API it only issues `get` and `list` requests, plus `get` on `services/proxy` when it queries an in-cluster Prometheus through the API server. The minimum permissions are in [deploy/rbac.yaml](deploy/rbac.yaml).
- Towards Prometheus it only issues instant queries (`GET /api/v1/query`).
- It sends no data to any other destination and has no telemetry.

Things that are particularly interesting to hear about:

- any code path that could modify cluster state,
- leaking credentials (kubeconfig contents, bearer tokens from `--prometheus-token-file`) into output or error messages,
- output injection through attacker-controlled names or metric labels (for example terminal escape sequences in the table, or markup in `-o markdown`).

Note that granting `services/proxy` on a Prometheus Service lets the holder read everything that Prometheus stores. That is a property of the permission, not of this tool, but scope it deliberately.
