#!/bin/bash

ENVIRONMENT=dev

RELEASE=${ENVIRONMENT}-zot
NAMESPACE=${ENVIRONMENT}-oci
VALUES=${ENVIRONMENT}.yaml

helm repo add project-zot http://zotregistry.dev/helm-charts

helm repo update

helm upgrade --install ${RELEASE} project-zot/zot --namespace ${NAMESPACE} --create-namespace --values ${VALUES}