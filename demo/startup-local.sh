#!/bin/bash

# This script is used to start the demo environment. It will start the required infrastructure as well as the agent so that the demo can be easily run.
ENVIRONMENT="dev"

# Remove any kind cluster that might already exist
kind delete clusters --all

# Create the KIND cluster
kind create cluster --config ../definitions/infra/kind-config.yaml

kubectl config use-context kind-gitless-gitops

# Wait for the cluster to be ready
while ! kubectl get nodes &> /dev/null; do
  echo "Waiting for the cluster to be ready..."
  sleep 5
done

echo "KIND cluster is ready. Proceeding with the setup..."

# Install the OCI registry inside cluster
NAMESPACE_OCI=${ENVIRONMENT}-oci
sh ../definitions/infra/oci/install-oci.sh "$ENVIRONMENT" "$NAMESPACE_OCI"

REGISTRY_SVC=$(kubectl get svc -n "${NAMESPACE_OCI}" -o jsonpath='{.items[0].metadata.name}')

echo "OCI registry is ready (SVC is ${REGISTRY_SVC} at ${NAMESPACE_OCI}). Forwarding to localhost:5000..."
kubectl port-forward svc/"${REGISTRY_SVC}" -n "${NAMESPACE_OCI}" 5000:80 &

echo "Uploading agent image to the OCI registry..."
docker build --no-cache -t localhost:5000/demo/agent:latest -f ../definitions/infra/gitops-agent/service/Containerfile ../definitions/infra/gitops-agent/service
docker push localhost:5000/demo/agent:latest

# Verify image is available in the registry
echo "Verifying agent image is available in the OCI registry..."
if ! curl -s -f -o /dev/null "http://localhost:5000/v2/demo/agent/manifests/latest"; then
  echo "Agent image is not available in the OCI registry."
  exit 1
fi

echo "Proceeding with the agent setup..."

# Install the GitOps agent
AGENT_NAMESPACE="${ENVIRONMENT}-agent"
sh ../definitions/infra/gitops-agent/install-agent.sh "${AGENT_NAMESPACE}" "${REGISTRY_SVC}.${NAMESPACE_OCI}.svc.cluster.local"
