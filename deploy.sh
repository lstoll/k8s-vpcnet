#!/usr/bin/env bash

if [ -z "$1" ]; then
    echo "--> Pass in path to kubeconfig as first arg"
    exit 1
fi

make release

VERSION=$(git rev-parse --short HEAD)$(if ! git diff-index --quiet HEAD --; then echo "-dirty"; fi)
TIMESTAMP=$(date +"%y%m%d%H%M%S")

cat manifest.yaml | sed -e "s/{{VERSION_TAG}}/${VERSION}/g" | sed -e "s/{{TIMESTAMP}}/${TIMESTAMP}/g" | kubectl --kubeconfig $1 apply -f -