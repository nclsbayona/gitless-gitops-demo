#!/bin/bash

# This script is used to install the App as OCI artifact in an OCI Registry.

NAMESPACE="$1"
REGISTRY_URL="$2"
API_IMAGE="demo/api:latest"
COWSAY_IMAGE="demo/cowsay:latest"
UI_IMAGE="demo/ui:latest"

# Split image names and tags
API_IMAGE_REPO=${API_IMAGE%:*}
API_IMAGE_TAG=${API_IMAGE#*:}
COWSAY_IMAGE_REPO=${COWSAY_IMAGE%:*}
COWSAY_IMAGE_TAG=${COWSAY_IMAGE#*:}
UI_IMAGE_REPO=${UI_IMAGE%:*}
UI_IMAGE_TAG=${UI_IMAGE#*:}

if [ -z "$NAMESPACE" ] || [ -z "$REGISTRY_URL" ]; then
  echo "Usage: $0 <NAMESPACE> <registry_service>"
  exit 1
fi

# VALUES="./app-values.yaml"
# cat <<EOF > ${VALUES}
# environment: ${NAMESPACE}

# registry_url: ${REGISTRY_URL}

# api_image:
#   repository: "${API_IMAGE_REPO}"
#   tag: "${API_IMAGE_TAG}"

# cowsay_image:
#   repository: "${COWSAY_IMAGE_REPO}"
#   tag: "${COWSAY_IMAGE_TAG}"

# ui_image:
#   repository: "${UI_IMAGE_REPO}"
#   tag: "${UI_IMAGE_TAG}"
# EOF

helm template --namespace "${NAMESPACE}" \
  --set environment="${NAMESPACE}" \
  --set registry_url="${REGISTRY_URL}" \
  --set api_image.repository="${API_IMAGE_REPO}" \
  --set api_image.tag="${API_IMAGE_TAG}" \
  --set cowsay_image.repository="${COWSAY_IMAGE_REPO}" \
  --set cowsay_image.tag="${COWSAY_IMAGE_TAG}" \
  --set ui_image.repository="${UI_IMAGE_REPO}" \
  --set ui_image.tag="${UI_IMAGE_TAG}" \
  ../definitions/app/helm/micro-app --output-dir rendered

mkdir bundle
cp rendered/micro-app/templates/*.yaml bundle/

GIT_COMMIT=$(git rev-parse HEAD)
GIT_AUTHOR=$(git log -1 --pretty=format:'%an <%ae>')
GIT_TIMESTAMP=$(git log -1 --pretty=format:'%cI')
GIT_BRANCH=$(git rev-parse --abbrev-ref HEAD)

oras push localhost:5000/demo/app:1.0.0 \
  $(find bundle -type f -exec printf '{}:application/yaml ' \;) \
  --annotation "org.opencontainers.image.created=$(date --iso-8601=seconds)" \
  --annotation "git.commit=${GIT_COMMIT}" \
  --annotation "git.author=${GIT_AUTHOR}" \
  --annotation "git.timestamp=${GIT_TIMESTAMP}" \
  --annotation "git.branch=${GIT_BRANCH}"
  --annotation "org.opencontainers.image.description=GitOps deployment bundle"

rm -rf bundle rendered
