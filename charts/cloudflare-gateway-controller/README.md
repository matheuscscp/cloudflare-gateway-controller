# cloudflare-gateway-controller

A Gateway API controller for Cloudflare tunnels.

## Installation

```bash
helm install cloudflare-gateway-controller -n ingress --create-namespace \
  oci://ghcr.io/matheuscscp/charts/cloudflare-gateway-controller \
  --set config.clusterName=my-cluster
```

The chart installs the Gateway API CRDs and admission policies by default.
If the Gateway API resources are already installed in the cluster, disable them:

```bash
helm install cloudflare-gateway-controller -n ingress --create-namespace \
  oci://ghcr.io/matheuscscp/charts/cloudflare-gateway-controller \
  --set config.clusterName=my-cluster \
  --skip-crds \
  --set api.enabled=false
```
