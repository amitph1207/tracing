#!/usr/bin/env bash
set -euo pipefail
cd "$(dirname "$0")"

IMG_CREATOR="tracing-demo-creator:local"
IMG_CONTROLLER="tracing-demo-controller:local"

go mod download
docker build -t "$IMG_CREATOR" -f Dockerfile.creator .
docker build -t "$IMG_CONTROLLER" -f Dockerfile.controller .

# Select the destination explicitly; a build alone does not modify a cluster.
case "${LOAD_TARGET:-none}" in
  none) ;;
  kind) kind load docker-image "$IMG_CREATOR" "$IMG_CONTROLLER" --name "${KIND_CLUSTER:-kind}" ;;
  minikube) minikube image load "$IMG_CREATOR"; minikube image load "$IMG_CONTROLLER" ;;
  k3d) k3d image import "$IMG_CREATOR" "$IMG_CONTROLLER" -c "${K3D_CLUSTER:-k3s-default}" ;;
  ssh)
    : "${REMOTE_NODES:?Set REMOTE_NODES, e.g. REMOTE_NODES='node-01 node-02'}"
    archive=$(mktemp /tmp/tracing-demo-images.XXXXXX.tar)
    trap 'rm -f "$archive"' EXIT
    docker save "$IMG_CREATOR" "$IMG_CONTROLLER" -o "$archive"
    for node in $REMOTE_NODES; do
      scp "$archive" "$node:/tmp/tracing-demo-images.tar"
      ssh "$node" 'sudo ctr -n k8s.io images import /tmp/tracing-demo-images.tar'
    done
    ;;
  *) echo 'LOAD_TARGET must be none, kind, minikube, k3d or ssh' >&2; exit 2 ;;
esac
printf 'Built %s and %s\n' "$IMG_CREATOR" "$IMG_CONTROLLER"
