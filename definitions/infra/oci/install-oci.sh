#!/bin/bash

ENVIRONMENT=$1
NAMESPACE=$2
if [ -z "$ENVIRONMENT" ] || [ -z "$NAMESPACE" ]; then
  echo "Usage: $0 <environment> <namespace>"
  exit 1
fi

if [ "$ENVIRONMENT" != "dev" ]; then
  echo "Invalid environment. Please use 'dev'."
  exit 1
fi

RELEASE=zot
VALUES=oci-values.yaml 

helm repo add project-zot http://zotregistry.dev/helm-charts

helm repo update

cat <<EOF > ${VALUES}
image:
  repository: ghcr.io/project-zot/zot-minimal-linux-amd64

service:
  type: ClusterIP
  port: 80
  clusterIP: 10.96.13.125
EOF

helm upgrade --install ${RELEASE} project-zot/zot --namespace ${NAMESPACE} --create-namespace --values ${VALUES}

# Check for pods to be ready
while ! kubectl get pods -n "${NAMESPACE}" | grep -q "1/1"; do
  echo "Waiting for OCI registry to be ready..."
  sleep 5
done

echo "Testing..."
helm test ${RELEASE} --namespace ${NAMESPACE}

if [ $? -ne 0 ]; then
  echo "OCI registry installation failed. Please check the logs."
  exit 1
fi