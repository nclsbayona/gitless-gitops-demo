#!/bin/bash

# This script is used to install the GitOps agent in the KIND cluster.
NAMESPACE="$1"
REGISTRY_URL="$2"
VALUES="./agent-values.yaml"

if [ -z "$NAMESPACE" ] || [ -z "$REGISTRY_URL" ]; then
  echo "Usage: $0 <NAMESPACE> <registry_service>"
  exit 1
fi

cat <<EOF > ${VALUES}
image:
  repository: ${REGISTRY_URL}/demo/agent

rules: |
  repository_url: zot.oci.svc.cluster.local/demo/app
  only: '^v.*$'

cosign_pub: LS0tLS1CRUdJTiBQVUJMSUMgS0VZLS0tLS0KTUZrd0V3WUhLb1pJemowQ0FRWUlLb1pJemowREFRY0RRZ0FFUzQwdm1sYktibEhXNW5rZ3E1SmsrNTgrQm1JVgpwNmREbEJVWC83a1lrVUU5OGE4RG9VR3dkZTN4RVFlQkJNQ3BMNGdNVE55aGswZFZINXFJNUxsWjN3PT0KLS0tLS1FTkQgUFVCTElDIEtFWS0tLS0tCg==
  
EOF

helm upgrade --install ${NAMESPACE} ../definitions/infra/gitops-agent/helm/agent --namespace ${NAMESPACE} --create-namespace --values ${VALUES}

# Check for pods to be ready
while ! kubectl get pods -n "${NAMESPACE}" | grep -q "1/1"; do
  echo "Waiting for GitOps Agent to be ready..."
  sleep 5
done

echo "Testing..."
helm test ${NAMESPACE} --namespace ${NAMESPACE}

if [ $? -ne 0 ]; then
  echo "GitOps Agent installation failed. Please check the logs."
  exit 1
fi