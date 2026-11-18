#!/bin/sh
# Creates the bucket of the local object storage (the storage service of
# compose.yaml). Safe to run again: an existing bucket is left alone.
set -eu

endpoint=http://127.0.0.1:9000
bucket=koroku
credentials=koroku-dev:koroku-dev-secret

tries=0
# Any HTTP answer (an unsigned request gets 403) means the gateway is up.
until curl -sS -o /dev/null "$endpoint/" 2>/dev/null; do
    tries=$((tries + 1))
    if [ "$tries" -ge 60 ]; then
        echo "dev/storage-setup.sh: $endpoint did not become ready" >&2
        exit 1
    fi
    sleep 1
done

# 200: created. 409: the bucket already exists (BucketAlreadyOwnedByYou).
status=$(curl -sS -o /dev/null -w '%{http_code}' -X PUT \
    --aws-sigv4 "aws:amz:us-east-1:s3" --user "$credentials" \
    "$endpoint/$bucket")
case $status in
    200) echo "dev/storage-setup.sh: created bucket $bucket" >&2 ;;
    409) echo "dev/storage-setup.sh: bucket $bucket exists" >&2 ;;
    *)
        echo "dev/storage-setup.sh: creating bucket $bucket failed (HTTP $status)" >&2
        exit 1
        ;;
esac
