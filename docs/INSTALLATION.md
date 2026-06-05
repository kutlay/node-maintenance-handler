# Installation Guide

This guide covers installation of the node-maintenance-controller on a Linode Kubernetes Engine (LKE) cluster using a single YAML manifest.
The controller image is published on Docker Hub: [`kutlay/node-maintenance-controller:latest`](https://hub.docker.com/r/kutlay/node-maintenance-controller)

## Prerequisites

- LKE cluster with `kubectl` configured
- A Linode API token with **Account — Read Only** scope

## Step 1: Create the Linode API Token

1. In Linode Cloud Manager go to **Profile → API Tokens → Create Personal Access Token**.
2. Grant **Account — Read Only** scope.
3. Copy the token value — you will not be able to view it again.

## Step 2: Install the Controller

`dist/install.yaml` bundles all Kubernetes manifests (Namespace, CRD, RBAC, Deployment) into a single file and already references the published image.

### Clone the repository

```sh
# Clone the repository
git clone https://github.com/kutlay/node-maintenance-controller.git
cd node-maintenance-controller
```

### Apply

```sh
# 1. Apply all manifests
kubectl apply -f dist/install.yaml

# 2. Create the token Secret in the namespace that was just created
kubectl create secret generic linode-token \
  --namespace node-maintenance-controller-system \
  --from-literal=token=<YOUR_LINODE_TOKEN>
```

### Customize controller flags

Edit the `args` section of the `Deployment` in `config/manager/manager.yaml`:

```yaml
args:
  - --leader-elect
  - --poll-interval=5m
  - --maintenance-window=30m
  - --post-maintenance-uncordon-delay=5m
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
