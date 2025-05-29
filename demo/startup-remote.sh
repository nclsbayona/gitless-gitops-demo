#!/bin/bash

# This script is used to start the demo environment. It will start the required infrastructure as well as the agent so that the demo can be easily run.
ENVIRONMENT="dev"

# Create the KIND cluster
kind cluster create --config ../infra/kind-config.yaml

kubectl config use-context kind-gitless-gitops

# Wait for the cluster to be ready
while ! kubectl get nodes &> /dev/null; do
  echo "Waiting for the cluster to be ready..."
  sleep 5
done

echo "KIND cluster is ready. Proceeding with the setup..."