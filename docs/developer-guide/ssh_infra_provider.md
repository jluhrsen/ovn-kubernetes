# SSH infrastructure provider

The SSH infrastructure provider exercises OVN-Kubernetes e2e tests against a
cluster whose **container runtime** is reachable over SSH. It is a **subset /
guardrail** provider: it drives the shared `container.Engine` with an SSH
`api.Runner` to manage external containers and container networks on the SSH
host.

For upstream development, this lets `go test` run on one machine while the KinD
cluster and its container runtime live on another Linux host, such as a
dedicated host with more CPU, memory, or disk, or a testbed with a particular
kernel, NIC, or other hardware. Remote-host support is partial: images must
already be present on that host because image preload via `kind load` is not
wired.

The standalone `Provider` targets **nodes-as-containers** topologies (KinD, or
any cluster whose Kubernetes nodes are containers on the same runtime daemon as
the SSH target). It is **not** a generic per-node SSH cluster provider.

A custom `NodeExecutor` only affects `ExecK8NodeCommand`; methods such as
`GetK8NodeNetworkInterface`, `StartNode`, and `ShutdownNode` remain
container-engine operations on the standalone provider. VM, bare-metal, and
cloud platforms still require a downstream full-provider adapter, which may use
`container.Engine` with its own `api.Runner`.

## Supported topology (upstream)

| Topology | Supported | Notes |
|----------|-----------|-------|
| KinD on the same machine as the test runner, reached via SSH to localhost | Yes | Primary supported topology |
| KinD on a remote host, kubeconfig on the runner points at that cluster | Partial | External-container ops use the remote runtime; image preload via `kind load` is **not** wired (images must be present on the remote host) |
| VM / bare-metal / cloud nodes | No (upstream) | Requires a downstream adapter implementing the full cluster provider with `container.Engine` and its own runner |

### Execution model

SSH is a **transport to the container-engine host**, not to every Kubernetes
node. The default node strategy (`OVN_TEST_SSH_NODE_EXECUTOR=container`) treats
each Kubernetes node as a **container on that same daemon** (`docker exec
<node>`). This matches KinD and similar single-daemon topologies.

Node lifecycle (`StartNode` / `ShutdownNode`) and `GetK8NodeNetworkInterface`
use the same container model.

## Environment variables

| Variable | Required | Default | Purpose |
|----------|----------|---------|---------|
| `OVN_TEST_INFRA_PROVIDER` | No | `kind` | Set to `ssh` to select this provider |
| `OVN_TEST_SSH_HOST` | Yes | — | SSH host running the container runtime (bare IP/hostname; no embedded port) |
| `OVN_TEST_SSH_USER` | Yes | — | SSH user |
| `OVN_TEST_SSH_KEY` | Yes | — | Path to SSH private key (`~` expansion supported) |
| `OVN_TEST_SSH_PORT` | No | `22` | SSH port |
| `OVN_TEST_SSH_NODE_EXECUTOR` | No | `container` | Node command strategy (only `container` today) |
| `OVN_TEST_SSH_KNOWN_HOSTS` | No | `~/.ssh/known_hosts` if present | Host-key verification |
| `OVN_TEST_SSH_INSECURE_HOST_KEY` | No | off | Set to `1` to skip host-key verification (test-only) |
| `OVN_TEST_PRIMARY_NETWORK` | No | `kind` | Primary container network name |
| `CONTAINER_RUNTIME` | No | `docker` | `docker` or `podman` on the SSH host |

## Capabilities and test subset

The SSH provider does **not** implement `SetupUnderlay` (localnet underlay
wiring). Specs that need it are skipped automatically when the active provider
reports `SetupUnderlay` as unsupported.

## Image preload

When **all** of the following hold, `TestMain` wires KinD's image preloader:

- `OVN_TEST_INFRA_PROVIDER=ssh`
- kubectl context is `kind-*`
- `OVN_TEST_SSH_HOST` is localhost (`127.0.0.1`, `::1`, or `localhost`)

Otherwise images must be loaded on the SSH host out of band.

## Example: localhost KinD setup

```bash
# After contrib/kind.sh / make -C test install-kind
export OVN_TEST_INFRA_PROVIDER=ssh
export OVN_TEST_SSH_HOST=127.0.0.1
export OVN_TEST_SSH_USER=$USER
export OVN_TEST_SSH_KEY=~/.ssh/id_ed25519
ssh-keyscan -H 127.0.0.1 >> ~/.ssh/known_hosts
export OVN_TEST_SSH_KNOWN_HOSTS=~/.ssh/known_hosts
```

## Security

Host-key verification is **on by default** for the SSH provider
(`NewSSHRunnerWithOptions`). Use `OVN_TEST_SSH_KNOWN_HOSTS` or ensure
`~/.ssh/known_hosts` contains the target. Set `OVN_TEST_SSH_INSECURE_HOST_KEY=1`
only for ephemeral local testing.

The legacy four-argument `NewSSHRunner` keeps insecure host-key verification for
compatibility; callers that require host-key verification should use
`NewSSHRunnerWithOptions`.

## Architecture

- `engine/container.Engine` — transport-agnostic external-container/network
  operations over an `api.Runner`
- `Provider` — standalone provider for nodes-as-containers topologies
- `engine/runner.NewSSHRunnerWithOptions` — SSH transport with configurable host-key policy
