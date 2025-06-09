#!/bin/bash

# Remove any kind cluster that might already exist
sudo kind delete clusters --all
# Clean podman cache
podman system prune -f --all