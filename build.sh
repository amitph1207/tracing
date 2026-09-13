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

docker save tracing-demo-creator:local -o /tmp/creator.tar
docker save tracing-demo-controller:local -o /tmp/controller.tar
scp /tmp/creator.tar node-01:/tmp/creator.tar
scp /tmp/controller.tar node-01:/tmp/controller.tar
ssh node-01 'sudo ctr -n k8s.io images import /tmp/creator.tar'
ssh node-01 'sudo ctr -n k8s.io images import /tmp/controller.tar'
ssh node-01 'sudo ctr -n k8s.io images ls | grep tracing-demo'

scp /tmp/creator.tar node-02:/tmp/creator.tar
scp /tmp/controller.tar node-02:/tmp/controller.tar
ssh node-02 'sudo ctr -n k8s.io images import /tmp/creator.tar'
ssh node-02 'sudo ctr -n k8s.io images import /tmp/controller.tar'
ssh node-02 'sudo ctr -n k8s.io images ls | grep tracing-demo'


echo "==> done. Images built:"
echo "    $IMG_CREATOR"
echo "    $IMG_CONTROLLER"
