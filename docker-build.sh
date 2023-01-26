#!/bin/bash

IMAGE=routeros-fletsv6-companion:armv7

set -e

docker build --platform linux/arm/v7 . -t "$IMAGE"