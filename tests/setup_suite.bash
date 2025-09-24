#!/bin/bash

set -eu

function setup_suite {
  export BATS_TEST_TIMEOUT=120
  # Define the name of the kind cluster
  export CLUSTER_NAME="knd-cluster"
  # Build the image
  docker build -t "$IMAGE_NAME":test -f Dockerfile "$BATS_TEST_DIRNAME"/.. --load

  mkdir -p _artifacts
  rm -rf _artifacts/*
  # create cluster
  kind create cluster \
    --name $CLUSTER_NAME      \
    -v7 --wait 1m --retain    \
    --config="$BATS_TEST_DIRNAME"/../kind.yaml

  TAG=stable make images
  kind load docker-image ghcr.io/gke-labs/dravip:stable
  kind load docker-image ghcr.io/gke-labs/dravip-controller:stable

  _install=$(sed s#"$IMAGE_NAME".*#"$IMAGE_NAME":test# < "$BATS_TEST_DIRNAME"/../install.yaml)
  printf '%s' "${_install}" | kubectl apply -f -
  kubectl wait --for=condition=ready pods --namespace=kube-system -l k8s-app=dravip
  kubectl wait --for=condition=ready pods --namespace=kube-system -l k8s-app=dravip-controller

}

function teardown_suite {
    kind export logs "$BATS_TEST_DIRNAME"/../_artifacts --name "$CLUSTER_NAME"
    kind delete cluster --name "$CLUSTER_NAME"
}