#!/usr/bin/env bash
set -euo pipefail

IMG_CREATOR="tracing-demo-creator:local"
IMG_CONTROLLER="tracing-demo-controller:local"

echo "==> go mod tidy (generates go.sum)"
go mod tidy

echo "==> building creator image"
docker build -t "$IMG_CREATOR" -f Dockerfile.creator .

echo "==> building controller image"
docker build -t "$IMG_CONTROLLER" -f Dockerfile.controller .

# Load into whichever local cluster tool is available, so you don't need
# a registry for this demo.
if command -v kind >/dev/null 2>&1 && kind get clusters >/dev/null 2>&1; then
  CLUSTER="$(kind get clusters | head -n1)"
  echo "==> loading images into kind cluster '$CLUSTER'"
  kind load docker-image "$IMG_CREATOR" --name "$CLUSTER"
  kind load docker-image "$IMG_CONTROLLER" --name "$CLUSTER"
elif command -v minikube >/dev/null 2>&1 && minikube status >/dev/null 2>&1; then
  echo "==> loading images into minikube"
  minikube image load "$IMG_CREATOR"
  minikube image load "$IMG_CONTROLLER"
elif command -v k3d >/dev/null 2>&1 && k3d cluster list >/dev/null 2>&1; then
  CLUSTER="$(k3d cluster list -o json | jq -r '.[0].name')"
  echo "==> importing images into k3d cluster '$CLUSTER'"
  k3d image import "$IMG_CREATOR" "$IMG_CONTROLLER" -c "$CLUSTER"
else
  echo "==> no local cluster tool auto-detected (kind/minikube/k3d)."
  echo "    push $IMG_CREATOR and $IMG_CONTROLLER to a registry your"
  echo "    cluster can pull from instead, and update manifests/*.yaml"
  echo "    image: fields accordingly."
fi

echo "==> done. Images built:"
echo "    $IMG_CREATOR"
echo "    $IMG_CONTROLLER"
