#!/usr/bin/env bash
# Build and publish demo images to ttl.sh for the current CI run.
# The image tags are written to GITHUB_ENV so subsequent steps can reference
# PUBLISH_IMAGE and PUBLISH_CONTROL_PLANE_IMAGE.
#
# Required environment variables (set in the workflow):
#   IMAGE_PREFIX, CONTROL_IMAGE_PREFIX  — base names for the images
#   INTEGRATION                          — integration directory name, e.g. cluster-router-eg
#   GITHUB_RUN_ID, GITHUB_RUN_ATTEMPT   — provided by GitHub Actions
set -euxo pipefail

image_tag="${GITHUB_RUN_ID}-${GITHUB_RUN_ATTEMPT}"
publish_image="ttl.sh/${IMAGE_PREFIX}-${image_tag}:1h"
publish_control_image="ttl.sh/${CONTROL_IMAGE_PREFIX}-${image_tag}:1h"

echo "PUBLISH_IMAGE=${publish_image}" >> "${GITHUB_ENV}"
echo "PUBLISH_CONTROL_PLANE_IMAGE=${publish_control_image}" >> "${GITHUB_ENV}"

make -C "integrations/${INTEGRATION}" publish \
  PUBLISH_IMAGE="${publish_image}" \
  PUBLISH_CONTROL_PLANE_IMAGE="${publish_control_image}"
