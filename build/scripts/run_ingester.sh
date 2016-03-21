#!/bin/bash --
set -e -u -o pipefail

cd -- "$(dirname -- "$0")"
eval "$(curl 169.254.169.254/latest/user-data/)"
export HOST="$(curl 169.254.169.254/latest/meta-data/hostname)"
export STATSD_HOSTPORT="localhost:8125"
export CONFIG_PREFIX="s3://$S3_CONFIG_BUCKET/$VPC_SUBNET_TAG/$CLOUD_APP/$CLOUD_ENVIRONMENT"
NO_WORK_DELAY="1m"  # Overidable in conf.sh
aws s3 cp --region us-west-2 "$CONFIG_PREFIX/conf.sh" conf.sh
source conf.sh

exec ./rs_ingester \
  -n_workers 5 \
  -statsPrefix="${CLOUD_APP}.${CLOUD_DEV_PHASE:-${CLOUD_ENVIRONMENT}}.${EC2_REGION}.${CLOUD_AUTO_SCALE_GROUP##*-}" \
  -databaseURL="${INGESTER_DB_URL}" \
  -manifestBucketPrefix="rsingester-manifests" \
  -loadCountTrigger="${LOAD_COUNT_TRIGGER}" \
  -loadAgeSeconds="${LOAD_AGE_SECONDS}" \
  -tableWhitelist="${TABLE_WHITELIST}" \
  -no_work_delay="${NO_WORK_DELAY}" \
  -rsURL="${RS_URL}"
