#!/usr/bin/env bash
set -euo pipefail

REDIS_ADDR="${REDIS_ADDR:-redis.gray-demo.svc.cluster.local:6379}"

redis-cli -h "${REDIS_ADDR%:*}" -p "${REDIS_ADDR##*:}" SET gray:whitelist:pc:enabled 1
redis-cli -h "${REDIS_ADDR%:*}" -p "${REDIS_ADDR##*:}" DEL gray:whitelist:pc:tenantId gray:whitelist:pc:userId
redis-cli -h "${REDIS_ADDR%:*}" -p "${REDIS_ADDR##*:}" SADD gray:whitelist:pc:tenantId 2

echo 'gray demo Redis data initialized'
