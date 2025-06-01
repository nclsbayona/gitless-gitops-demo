#!/bin/bash
# This script builds and pushes the microservices for the App to an OCI registry.
MICROSERVICE=$1
REGISTRY_URL=$2
TAG="${3:-latest}"

if [ -z "$MICROSERVICE" ] || [ -z "$REGISTRY_URL" ]; then
  echo "Usage: $0 <MICROSERVICE> <REGISTRY_URL> [<TAG>]"
  exit 1
fi

echo "Building and pushing microservice: $MICROSERVICE to registry: $REGISTRY_URL with tag: $TAG"

podman build --no-cache -t "demo/${MICROSERVICE}:${TAG}" -f "../definitions/app/microservices/${MICROSERVICE}/Containerfile" "../definitions/app/microservices/${MICROSERVICE}"

if [ $? -ne 0 ]; then
  echo "Failed to build microservice: $MICROSERVICE"
  exit 1
fi
echo "Pushing microservice: $MICROSERVICE to registry: $REGISTRY_URL"
skopeo copy \
  --dest-tls-verify=false \
  --format oci \
  containers-storage:localhost/demo/${MICROSERVICE}:${TAG} \
  docker://${REGISTRY_URL}/demo/${MICROSERVICE}:${TAG}