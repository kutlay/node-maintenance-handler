# Installation Guide

This guide covers two ways to deploy the node-maintenance-controller on a Linode Kubernetes Engine (LKE) cluster:

- **[Option A — Helm](#option-a--helm-chart)** (recommended): full lifecycle management, easy upgrades, configurable via `values.yaml`
- **[Option B — Single YAML](#option-b--single-yaml-file)**: one-command apply, no Helm required

The controller image is published on Docker Hub: [`kutlay/node-maintenance-controller:latest`](https://hub.docker.com/r/kutlay/node-maintenance-controller)

---

## Prerequisites

- LKE cluster with `kubectl` configured
- **Option A only**: Helm v3.8+
- A Linode API token with **Account — Read Only** scope

---

## Step 1: Create the Linode API Token

1. In Linode Cloud Manager go to **Profile → API Tokens → Create Personal Access Token**.
2. Grant **Account — Read Only** scope.
3. Copy the token value — you will not be able to view it again.

---

## Option A — Helm Chart

The Helm chart lives in `dist/chart/` and is generated from the same Kustomize manifests that back the single-YAML option.

### Install

```sh
# Clone the repository
git clone https://github.com/kutlay/node-maintenance-controller.git
cd node-maintenance-controller

# Install the chart
helm install node-maintenance-controller dist/chart/ \
  --namespace node-maintenance-controller-system \
  --create-namespace
```

The chart creates the namespace, CRD, ServiceAccount, RBAC, and the controller Deployment using the published image by default.

> **Token Secret**: the chart does not manage the Secret (tokens are sensitive). Create it in the release namespace after `helm install`:
>
> ```sh
> kubectl create secret generic linode-token \
>   --namespace node-maintenance-controller-system \
>   --from-literal=token=<YOUR_LINODE_TOKEN>
> ```

### Configure with a values file

Create a `my-values.yaml` to override defaults instead of using many `--set` flags:

```yaml
manager:
  image:
    repository: kutlay/node-maintenance-controller
    tag: "latest"

  # Controller flags — add any flag the manager binary accepts
  args:
    - --leader-elect
    - --poll-interval=5m
    - --maintenance-window=48h
    - --post-maintenance-uncordon-delay=2m
    - --linode-token-secret-name=linode-token
    # --linode-token-secret-namespace is auto-resolved from POD_NAMESPACE (Downward API)
```

```sh
helm install node-maintenance-controller dist/chart/ \
  --namespace node-maintenance-controller-system \
  --create-namespace \
  -f my-values.yaml
```

### Key `values.yaml` fields

| Field | Default | Description |
|---|---|---|
| `manager.image.repository` | `kutlay/node-maintenance-controller` | Image registry/name |
| `manager.image.tag` | Chart `appVersion` | Image tag |
| `manager.image.pullPolicy` | `IfNotPresent` | Image pull policy |
| `manager.args` | `[--leader-elect]` | Extra arguments passed to the manager binary |
| `manager.replicas` | `1` | Number of controller replicas |
| `manager.resources` | 500m/128Mi limits | CPU/memory limits and requests |
| `crd.enable` | `true` | Install the `NodeMaintenance` CRD with the chart |
| `crd.keep` | `true` | Retain the CRD (and all `NodeMaintenance` objects) on uninstall |
| `metrics.enable` | `true` | Expose `/metrics` endpoint on `:8443` |
| `metrics.secure` | `true` | Serve metrics over HTTPS with authn/authz |
| `prometheus.enable` | `false` | Create a `ServiceMonitor` for Prometheus Operator |

### Controller flags reference

Pass these as entries under `manager.args`:

| Flag | Default | Description |
|---|---|---|
| `--linode-token-secret-name` | `linode-token` | Name of the Secret holding the Linode API token |
| `--linode-token-secret-key` | `token` | Key inside the Secret |
| `--poll-interval` | `10m` | How often the Linode Maintenance API is polled |
| `--maintenance-window` | `24h` | Nodes with maintenance within this window are flagged |
| `--post-maintenance-uncordon-delay` | `0s` | Wait after maintenance ends before uncordoning |

### Upgrade

```sh
helm upgrade node-maintenance-controller dist/chart/ \
  --namespace node-maintenance-controller-system \
  -f my-values.yaml
```

### Uninstall

```sh
helm uninstall node-maintenance-controller --namespace node-maintenance-controller-system

# The CRD and all NodeMaintenance objects are kept by default (crd.keep=true).
# To also delete the CRD:
kubectl delete crd nodemaintenances.maintenance.linode.com
```

> Node labels, taints, and conditions applied to Nodes are **not** removed automatically. Remove them manually with `kubectl label`, `kubectl taint`, or `kubectl edit node`.

---

## Option B — Single YAML File

`dist/install.yaml` bundles all Kubernetes manifests (Namespace, CRD, RBAC, Deployment) into a single file and already references the published image.

### Apply

```sh
# 1. Apply all manifests
kubectl apply -f dist/install.yaml

# 2. Create the token Secret in the namespace that was just created
kubectl create secret generic linode-token \
  --namespace node-maintenance-controller-system \
  --from-literal=token=<YOUR_LINODE_TOKEN>
```

Or apply directly from the repository without cloning:

```sh
kubectl apply -f https://raw.githubusercontent.com/kutlay/node-maintenance-controller/main/dist/install.yaml

kubectl create secret generic linode-token \
  --namespace node-maintenance-controller-system \
  --from-literal=token=<YOUR_LINODE_TOKEN>
```

### Customise controller flags

Edit the `args` section of the `Deployment` in `config/manager/manager.yaml`:

```yaml
args:
  - --leader-elect
  - --health-probe-bind-address=:8081
  - --poll-interval=5m
  - --maintenance-window=48h
  - --post-maintenance-uncordon-delay=2m
```

Then rebuild:

```sh
make build-installer
kubectl apply -f dist/install.yaml
```

### Uninstall

```sh
kubectl delete -f dist/install.yaml

# Delete the token Secret separately
kubectl delete secret linode-token -n node-maintenance-controller-system
```

> Node labels, taints, and conditions applied to Nodes are **not** removed automatically.

---

## Step 2: Verify the Installation

```sh
# Controller pod is Running
kubectl get pods -n node-maintenance-controller-system

# CRD is registered
kubectl get crd nodemaintenances.maintenance.linode.com

# Controller logs (look for "Starting Linode maintenance poller")
kubectl logs -n node-maintenance-controller-system \
  deployment/node-maintenance-controller-controller-manager \
  -c manager -f
```

Once a poll cycle completes and Linode reports upcoming maintenance for a node, `NodeMaintenance` objects appear:

```sh
kubectl get nodemaintenances
```

```
NAME              NODE              LINODEID   WINDOW                 PHASE     AGE
lke-12345-node1   lke-12345-node1   12345      2026-06-01T04:00:00Z   Pending   5m
```

Affected nodes receive:

| Signal | Key/Type | Value |
|---|---|---|
| Label | `maintenance.linode.com/upcoming-maintenance` | RFC3339 scheduled time (`:` replaced by `.`) |
| Taint | `maintenance.linode.com/upcoming-maintenance` | `NoSchedule` |
| Node Condition | `UpcomingMaintenance` | `True` |

---

## Security Note

The controller's `ClusterRole` grants cluster-wide `get` on `secrets`. For production, add a supplemental `Role` that restricts access to only the specific Secret:

```yaml
apiVersion: rbac.authorization.k8s.io/v1
kind: Role
metadata:
  name: linode-token-reader
  namespace: node-maintenance-controller-system
rules:
- apiGroups: [""]
  resources: ["secrets"]
  resourceNames: ["linode-token"]
  verbs: ["get"]
```
