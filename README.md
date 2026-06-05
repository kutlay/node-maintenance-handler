# node-maintenance-controller


## Overview

Node Maintenance Controller is a Kubernetes controller for [Linode Kubernetes Engine (LKE)](https://www.akamai.com/products/kubernetes) that automatically detects upcoming Linode infrastructure maintenance and safely prepares your cluster nodes before the maintenance window arrives.

Linode periodically performs infrastructure maintenance on the physical hosts that back LKE nodes (reboots, hardware replacements, etc.). By default, Kubernetes has no awareness of these events — workloads are not drained, nodes are not cordoned, and the maintenance often causes unexpected pod disruptions.

**Node Maintenance Controller** closes this gap. It polls the [Linode maintenance API](https://techdocs.akamai.com/linode-api/reference/get-maintenance) on a configurable interval and, for any node whose maintenance window falls within a configurable look-ahead duration, it:

1. Creates a `NodeMaintenance` custom resource that tracks the lifecycle of the event (`Pending` → `Active` → `Completed`).
2. Applies three signals to the affected Kubernetes `Node` so that external tooling (e.g. [Node Healthcheck Operator](https://github.com/medik8s/node-healthcheck-operator), custom scripts, dashboards) can react:
   - **Label** `maintenance.linode.com/upcoming-maintenance=true`
   - **Label** `maintenance.linode.com/window-start=<RFC3339 timestamp>`
   - **Taint** `maintenance.linode.com/upcoming-maintenance:NoSchedule`
   - **Node Condition** `UpcomingMaintenance=True`
3. Optionally **cordons** the node to stop new workloads from being scheduled.
4. Optionally **drains** the node, evicting existing pods gracefully before the maintenance window starts — with configurable retry logic and timeouts.
5. **Automatically cleans up** all signals, uncordons the node (if it was schedulable before the maintenance), and deletes the `NodeMaintenance` object once Linode reports the maintenance as complete.


### How It Works

```
Linode API  ──poll──▶  Maintenance Poller
                              │
                    create / update / complete
                              │
                              ▼
                    NodeMaintenance CRD   ◀── reconciled by ──▶  NodeMaintenance Controller
                                                                        │
                                              apply / remove signals    │
                                              (label, taint, condition) │
                                              cordon / drain            │
                                                                        ▼
                                                               Kubernetes Node
```

The **poller** owns phase transitions (`Pending` → `Active` → `Completed`) and is the authoritative link to Linode. The **reconciler** owns all mutations to the Node and reacts to phase changes. The two components are fully decoupled.

### Node Signals

When maintenance is detected, the following signals are applied to the `Node`:

| Signal | Key | Value |
|---|---|---|
| Label | `maintenance.linode.com/upcoming-maintenance` | `true` |
| Label | `maintenance.linode.com/window-start` | RFC3339 timestamp (`:` replaced with `.` for label compatibility) |
| Taint | `maintenance.linode.com/upcoming-maintenance` | Effect: `NoSchedule` |
| Node Condition | `UpcomingMaintenance` | `True` — message includes scheduled time and maintenance type |

All signals are removed automatically once maintenance completes.

### NodeMaintenance Lifecycle

```
(Linode maintenance detected)
         │
         ▼
      Pending   ← maintenance scheduled, node outside maintenance window, signals applied
         │
         ▼
      Active    ← node is within maintenance window; if cordon/drain enabled, node is cordoned and drained at this point
         │
         ▼
     Completed  ← Linode no longer lists this maintenance; signals removed, NodeMaintenance CR deleted
```

### Configuration

The controller's behaviour is controlled entirely through command-line flags. Key knobs:

| Concern | Flags |
|---|---|
| Linode API credentials | `--linode-token-secret-name`, `--linode-token-secret-namespace` |
| Poll frequency | `--poll-interval` (default: `10m`) |
| How far ahead to act | `--maintenance-window` (default: `24h`) |
| Cordon nodes | `--cordon-nodes` |
| Drain nodes | `--drain-nodes`, `--drain-timeout`, `--drain-max-retries`, `--drain-retry-interval` |
| Post-maintenance delay | `--post-maintenance-uncordon-delay` |

See [docs/FLAGS.md](docs/FLAGS.md) for the full flag reference.

## Installation

There are two options to install this project on a Kubernetes cluster:

1. Using a Helm chart (see [docs/HELM_INSTALLATION.md](docs/HELM_INSTALLATION.md))
2. Using a single YAML bundle (see [docs/INSTALLATION.md](docs/INSTALLATION.md))

The controller image is published on Docker Hub: [`kutlay/node-maintenance-controller:latest`](https://hub.docker.com/r/kutlay/node-maintenance-controller)

### Quick Start (Helm)

```sh
git clone https://github.com/kutlay/node-maintenance-controller.git
cd node-maintenance-controller

helm install node-maintenance-controller dist/chart/ \
  --namespace node-maintenance-controller-system \
  --create-namespace \
  --set manager.args[0]="--poll-interval=10m" \
  --set manager.args[1]="--maintenance-window=24h" \
  --set manager.args[2]="--cordon-nodes"

kubectl create secret generic linode-token \
  --namespace node-maintenance-controller-system \
  --from-literal=token=<YOUR_LINODE_TOKEN>
```

A Linode API token with **Account (Read Only)** scope is required.

## Contributing

Contributions are welcome. To get started:

1. Fork the repository and create a feature branch.
2. Make your changes — follow the [Kubebuilder conventions](https://book.kubebuilder.io/reference/good-practices.html) for controller and API changes.
3. After editing `*_types.go` or kubebuilder markers, regenerate manifests:
   ```sh
   make manifests
   make generate
   ```
4. Run linting and tests before opening a pull request:
   ```sh
   make lint-fix
   make test
   ```
5. Open a pull request with a clear description of the change and the motivation behind it.