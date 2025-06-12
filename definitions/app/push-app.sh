#!/bin/bash

# This script is used to install the App as OCI artifact in an OCI Registry.

NAMESPACE="$1"
REGISTRY_URL="$2"
TAG="${3:-latest}"
LOCAL_REGISTRY="${4:-$REGISTRY_URL}"
API_IMAGE="${5:-demo/api}:$(cat ../definitions/app/versions.txt | grep api | awk -F '=' '{print $2}')"
COWSAY_IMAGE="${6:-demo/cowsay}:$(cat ../definitions/app/versions.txt | grep cowsay | awk -F '=' '{print $2}')"
UI_IMAGE="${7:-demo/ui}:$(cat ../definitions/app/versions.txt | grep ui | awk -F '=' '{print $2}')"

# Split image names and tags
API_IMAGE_REPO=${API_IMAGE%:*}
API_IMAGE_TAG=${API_IMAGE#*:}
COWSAY_IMAGE_REPO=${COWSAY_IMAGE%:*}
COWSAY_IMAGE_TAG=${COWSAY_IMAGE#*:}
UI_IMAGE_REPO=${UI_IMAGE%:*}
UI_IMAGE_TAG=${UI_IMAGE#*:}

if [ -z "$NAMESPACE" ] || [ -z "$REGISTRY_URL" ]; then
  echo "Usage: $0 <NAMESPACE> <registry_service> [<LOCAL_REGISTRY> <TAG> <API_IMAGE> <COWSAY_IMAGE> <UI_IMAGE>]"
  exit 1
fi

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

# Check for cosign.key and create if needed
if [ ! -f "../demo/cosign.key" ]; then
    if [ -z "$COSIGN_KEY" ]; then
        echo "Error: cosign.key not found and COSIGN_KEY environment variable is not set"
        exit 1
    fi
    echo "Creating cosign.key from environment variable..."
    echo "$COSIGN_KEY" | base64 -d > "../demo/cosign.key"
    if [ $? -ne 0 ]; then
        echo "Error: Failed to decode COSIGN_KEY"
        exit 1
    fi
fi

GIT_COMMIT_HASH=$(git rev-parse HEAD)
GIT_COMMIT_MSG=$(git log -1 --pretty=format:'%s')
GIT_AUTHOR=$(git log -1 --pretty=format:'%an <%ae>')
GIT_TIMESTAMP=$(git log -1 --pretty=format:'%cI')
GIT_BRANCH=$(git rev-parse --abbrev-ref HEAD)

STAGING_TAG="${LOCAL_REGISTRY}/demo/app:pre-${TAG}"
FINAL_TAG="${LOCAL_REGISTRY}/demo/app:${TAG}"

# First push to staging tag
echo "Pushing bundle to staging tag ${STAGING_TAG}..."
oras push ${STAGING_TAG} \
  $(find bundle -type f -exec printf '{}:application/yaml ' \;) \
  --annotation "org.opencontainers.image.created=$(date --iso-8601=seconds)" \
  --annotation "git.commit.hash=${GIT_COMMIT_HASH}" \
  --annotation "git.commit.message=${GIT_COMMIT_MSG}" \
  --annotation "git.author=${GIT_AUTHOR}" \
  --annotation "git.timestamp=${GIT_TIMESTAMP}" \
  --annotation "git.branch=${GIT_BRANCH}" \
  --annotation "org.opencontainers.image.description=GitOps deployment bundle" \
  --plain-http

if [ $? -ne 0 ]; then
    echo "Error: Failed to push bundle to staging tag ${STAGING_TAG}"
    rm -rf bundle rendered
    exit 1
fi

# Display artifact metadata and annotations
echo "Fetching artifact metadata for ${STAGING_TAG}..."
echo "Manifest:"
oras manifest fetch ${STAGING_TAG} --plain-http | jq '.'

# Get image digest for signing
echo -e "\nGetting image digest..."
# STAGING_DIGEST=$(oras manifest fetch ${STAGING_TAG} --plain-http | jq '.config.digest' | tr -d '"')
STAGING_DIGEST=$(curl -sI "${LOCAL_REGISTRY}/v2/demo/app/manifests/pre-${TAG}" | grep -i "docker-content-digest" | awk '{print $2}' | tr -d '\r')
if [ $? -ne 0 ] || [ -z "$STAGING_DIGEST" ]; then
    echo "Error: Failed to get image digest"
    rm -rf bundle rendered
    exit 1
fi

# Sign the staging artifact using digest
echo "Signing staging artifact..."
COSIGN_PASSWORD="" COSIGN_YES=true cosign sign --key "../demo/cosign.key" "${LOCAL_REGISTRY}/demo/app@${STAGING_DIGEST}" --allow-insecure-registry

if [ $? -ne 0 ]; then
    echo "Error: Failed to sign staging artifact"
    rm -rf bundle rendered
    exit 1
fi

# Verify staging signature using digest
echo "Verifying staging signature..."
cosign verify --key "../demo/cosign.pub" "${STAGING_TAG}" --allow-insecure-registry
if [ $? -ne 0 ]; then
    echo "Error: Staging signature verification failed"
    rm -rf bundle rendered
    exit 1
fi
echo "Staging signature verification successful"

if [ $? -ne 0 ]; then
    echo "Error: Failed to sign bundle"
    rm -rf ./.bundle-layout bundle rendered
    exit 1
fi

# Promote to final tag
echo "Promoting artifact from ${STAGING_TAG} to ${FINAL_TAG}..."
oras copy \
  ${STAGING_TAG} ${FINAL_TAG} \
  --from-plain-http \
   --to-plain-http 

if [ $? -ne 0 ]; then
    echo "Error: Failed to promote to final tag"
    rm -rf bundle rendered
    exit 1
fi

# Display final artifact metadata and annotations
echo "Fetching artifact metadata for ${FINAL_TAG}..."
echo "Manifest:"
oras manifest fetch ${FINAL_TAG} --plain-http | jq '.'

# Get final digest and verify signature
echo -e "\nGetting final digest for verification..."
FINAL_DIGEST=$(curl -sI "${LOCAL_REGISTRY}/v2/demo/app/manifests/${TAG}" | grep -i "docker-content-digest" | awk '{print $2}' | tr -d '\r')
if [ $? -ne 0 ] || [ -z "$FINAL_DIGEST" ]; then
    echo "Error: Failed to get final digest"
    rm -rf bundle rendered
    exit 1
fi
# Extract registry host and path from STAGING_TAG
REGISTRY_HOST=$(echo ${STAGING_TAG} | cut -d'/' -f1)
IMAGE_PATH=$(echo ${STAGING_TAG} | cut -d'/' -f2-)
TAG_NAME=$(echo ${IMAGE_PATH} | cut -d':' -f2)

echo "Current tags before cleanup:"
curl -s "http://${REGISTRY_HOST}/v2/demo/app/tags/list" | jq '.'

# Clean up staging tag using Registry API
echo "Cleaning up staging tag..."
curl -X DELETE "http://${REGISTRY_HOST}/v2/demo/app/manifests/pre-${TAG}"
if [ $? -ne 0 ]; then
    echo "Warning: Failed to clean up staging tag ${STAGING_TAG}"
fi

echo "Current tags after cleanup:"
curl -s "http://${REGISTRY_HOST}/v2/demo/app/tags/list" | jq '.'

echo "Verifying final signature..."
cosign verify --key ../demo/cosign.pub "${FINAL_TAG}" --allow-insecure-registry
if [ $? -ne 0 ]; then
    echo "Error: Failed to verify signature for final artifact"
    rm -rf bundle rendered
    exit 1
fi
echo "Signature verification successful for final artifact"

# Cleanup
rm -rf ./.bundle-layout bundle rendered