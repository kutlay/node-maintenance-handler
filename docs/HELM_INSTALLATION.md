# Installation Guide

This guide covers installation of the node-maintenance-controller on a Linode Kubernetes Engine (LKE) cluster using a HELM chart.

The controller image is published on Docker Hub: [`kutlay/node-maintenance-controller:latest`](https://hub.docker.com/r/kutlay/node-maintenance-controller)

---

## Prerequisites

- LKE cluster with `kubectl` configured
- Helm v3.8+
- A Linode API token with **Account — Read Only** scope

---

## Step 1: Create the Linode API Token

1. In Linode Cloud Manager go to **Profile → API Tokens → Create Personal Access Token**.
2. Grant **Account — Read Only** scope.
3. Copy the token value — you will not be able to view it again.

---

## Step 2: Install with Helm

The Helm chart lives in `dist/chart/` and is generated from the same Kustomize manifests that back the single-YAML option.

### Clone the repository

```sh
# Clone the repository
git clone https://github.com/kutlay/node-maintenance-controller.git
cd node-maintenance-controller
```

### Configure with a values file

You can create a `custom-values.yaml` to override defaults instead of using many `--set` flags:

```yaml
manager:
  image:
    repository: kutlay/node-maintenance-controller
    tag: "latest"

  # Controller flags — add any flag the manager binary accepts
  args:
    - --leader-elect
    - --poll-interval=10m # poll maintenance api every 10 minutes
    - --maintenance-window=30m # cordon 30m before maintenance
    - --post-maintenance-uncordon-delay=10m # uncordon 10m after maintenance is complete
    - --linode-token-secret-name=linode-token 
    - --linode-api-endpoint=https://api.linode.com

```

> You can find all available controller flags in the [FLAGS.md](FLAGS.md) reference.

### Install the chart

```sh
helm install node-maintenance-controller dist/chart/ \
  --namespace node-maintenance-controller-system \
  --create-namespace \
  -f custom-values.yaml
```

## Step 3: Create the Token Secret

The helm chart does not manage the secret that contains the Linode API token (tokens are sensitive). Create it in the release namespace after helm install:

```sh
kubectl create secret generic linode-token \
  --namespace node-maintenance-controller-system \
  --from-literal=token=<YOUR_LINODE_TOKEN>
```

### Upgrade

To upgrade the chart with new values:

```sh
helm upgrade node-maintenance-controller dist/chart/ \
  --namespace node-maintenance-controller-system \
  -f custom-values.yaml
```

### Uninstall

```sh
helm uninstall node-maintenance-controller --namespace node-maintenance-controller-system
kubectl delete secret linode-token -n node-maintenance-controller-system
```

The CRD and all NodeMaintenance objects are kept by default (crd.keep=true).
To also delete the CRD:

```sh
kubectl delete crd nodemaintenances.maintenance.linode.com
```

> Node labels, taints, and conditions applied to Nodes are **not** removed automatically. Remove them manually with `kubectl label`, `kubectl taint`, or `kubectl edit node`.
