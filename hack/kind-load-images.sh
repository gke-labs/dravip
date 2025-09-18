#!/bin/bash

set -o errexit
set -o nounset
set -o pipefail

REPO_ROOT=$(dirname "${BASH_SOURCE[0]}")/..

cd $REPO_ROOT

TAG=stable make images
kind load docker-image aojea/dravip:stable
kind load docker-image aojea/dravip-controller:stable
kubectl delete -f install.yaml --ignore-not-found
kubectl apply -f install.yaml
kubectl wait --for=condition=ready pods --namespace=kube-system -l k8s-app=dravip-controller
kubectl wait --for=condition=ready pods --namespace=kube-system -l k8s-app=dravip