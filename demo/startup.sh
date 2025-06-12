#!/bin/bash

# This script is used to start the demo environment. It will start the required infrastructure as well as the agent so that the demo can be easily run.
ENVIRONMENT="dev"

#Restore cowsay version to 1.0.0
sed -i 's/^cowsay=1\.0\.1/cowsay=1.0.0/' ../definitions/app/versions.txt
# Remove any kind cluster that might already exist
sudo kind delete clusters --all

# Create the KIND cluster
KUBECONFIG=${HOME}/.kube/config
sudo kind create cluster --config ../definitions/infra/kind-config.yaml --kubeconfig "${KUBECONFIG}"

kubectl config use-context kind-gitless-gitops

# Wait for the cluster to be ready
while ! kubectl get nodes &> /dev/null; do
  echo "Waiting for the cluster to be ready..."
  sleep 5
done

echo "KIND cluster is ready. Proceeding with the setup..."

# Install the OCI registry inside cluster
sh ../definitions/infra/oci/install-oci.sh "dev" "oci"

kubectl create ns "dev"

echo "OCI registry is service is named zot and available at oci. Forwarding to localhost:80..."
sudo kubectl --kubeconfig "${KUBECONFIG}" port-forward svc/zot -n oci 80:80 &

# Upload the agent image to the OCI registry
echo "Uploading agent image to the OCI registry..."
if command -v podman &> /dev/null; then
  podman build --no-cache -t demo/agent:latest -f ../definitions/infra/gitops-agent/service/Containerfile ../definitions/infra/gitops-agent/service
  skopeo copy \
  --dest-tls-verify=false \
  --format oci \
  containers-storage:localhost/demo/agent:latest \
  docker://zot.oci.svc.cluster.local/demo/agent:latest
else
  echo "Missing podman"
  exit 1
fi

# Verify image is available in the registry
echo "Verifying agent image is available in the OCI registry..."
if ! curl -s -f -o /dev/null http://zot.oci.svc.cluster.local/v2/demo/agent/manifests/latest; then
  echo "Agent image is not available in the OCI registry."
  exit 1
fi

echo "Agent image OK. Installing Agent in cluster"

# Install the GitOps agent
AGENT_NAMESPACE="dev-agent"
sh ../definitions/infra/gitops-agent/install-agent.sh "dev-agent" "zot.oci.svc.cluster.local"

# Publish the first app artifact to the OCI registry using helm template so GitOps Agent can pick it up
echo "Publishing the first app artifact to the OCI registry..."

sh ../definitions/app/push-microservices.sh "api" "zot.oci.svc.cluster.local"
sh ../definitions/app/push-microservices.sh "cowsay" "zot.oci.svc.cluster.local"
sh ../definitions/app/push-microservices.sh "ui" "zot.oci.svc.cluster.local"
sh ../definitions/app/push-app.sh "dev" "zot.oci.svc.cluster.local" "v1.0.0"