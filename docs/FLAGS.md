## Configuration Flags

### Linode API

| Flag | Default | Purpose |
|---|---|---|
| `--linode-token-secret-name` | `linode-token` | Name of the Kubernetes Secret containing the Linode API token. |
| `--linode-token-secret-namespace` | *(POD_NAMESPACE env var)* | Namespace of the token Secret. Falls back to the `POD_NAMESPACE` env var injected by the Downward API. |
| `--linode-token-secret-key` | `token` | Key within the Secret that holds the token value. |
| `--linode-api-endpoint` | *(linodego default)* | Override the Linode API base URL. Useful for non-production environments or to simulate maintenance events. |

### Poller

| Flag | Default | Purpose |
|---|---|---|
| `--poll-interval` | `10m` | How often the Linode maintenance API is queried. |
| `--maintenance-window` | `24h` | Look-ahead duration. Nodes with maintenance scheduled within this window receive signals (label, taint, condition). |

### Node Signals & Lifecycle

| Flag | Default | Purpose |
|---|---|---|
| `--cordon-nodes` | `false` | Cordon nodes (`spec.unschedulable=true`) when maintenance signals are applied. Does not drain. Independent of `--drain-nodes`. |
| `--drain-nodes` | `false` | Drain nodes after cordoning. Implies `--cordon-nodes`. |
| `--post-maintenance-uncordon-delay` | `0` | Duration to wait after maintenance completes before uncordoning and cleaning up (e.g. `5m`). `0` means immediate. |

### Drain Behaviour

| Flag | Default | Purpose |
|---|---|---|
| `--drain-timeout` | `5m` | Per-attempt time limit for the drain operation. If pods have not terminated within this window the attempt is counted as failed. |
| `--drain-max-retries` | `5` | Maximum drain attempts before giving up. The node remains cordoned after exhaustion. `0` means unlimited retries. |
| `--drain-retry-interval` | `1m` | Wait between consecutive failed drain attempts. |
| `--drain-ignore-daemonsets` | `true` | Skip DaemonSet-owned pods during drain. Should almost always stay `true`; DaemonSet pods reschedule immediately anyway. |
| `--drain-delete-emptydir-data` | `false` | Allow evicting pods that use `emptyDir` volumes. Data in those volumes is lost on eviction. |

### Manager / Infrastructure

| Flag | Default | Purpose |
|---|---|---|
| `--leader-elect` | `false` | Enable leader election so only one replica is active at a time. Required when running multiple replicas. |
| `--health-probe-bind-address` | `:8081` | Address for the liveness and readiness probe endpoints. |
| `--metrics-bind-address` | `0` | Address for the Prometheus metrics endpoint. `0` disables it. Use `:8080` (HTTP) or `:8443` (HTTPS). |
| `--metrics-secure` | `true` | Serve metrics over HTTPS. Set to `false` to use plain HTTP. |
| `--metrics-cert-path` | *(empty)* | Directory containing the metrics server TLS certificate. If unset, controller-runtime generates a self-signed cert. |
| `--metrics-cert-name` | `tls.crt` | Filename of the metrics server certificate inside `--metrics-cert-path`. |
| `--metrics-cert-key` | `tls.key` | Filename of the metrics server private key inside `--metrics-cert-path`. |
| `--webhook-cert-path` | *(empty)* | Directory containing the webhook server TLS certificate. |
| `--webhook-cert-name` | `tls.crt` | Filename of the webhook server certificate. |
| `--webhook-cert-key` | `tls.key` | Filename of the webhook server private key. |
| `--enable-http2` | `false` | Enable HTTP/2 for the metrics and webhook servers. Disabled by default to avoid CVE-2023-44487 (Rapid Reset). |
