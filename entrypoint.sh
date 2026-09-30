#!/bin/sh
# entrypoint.sh — render the base Gravwell config from env vars, then hand off
# to docker-log-watcher which supervises gravwell_file_follow.
set -e

CONFIG_FILE="/opt/gravwell/etc/file_follow.conf"
CONFIG_TEMPLATE="/etc/gravwell/file_follow.conf.template"
CONF_D="/opt/gravwell/etc/file_follow.conf.d"

# ── Ensure overlay directory exists ──────────────────────────────────────────
mkdir -p "${CONF_D}"

# ── Render base config from template ─────────────────────────────────────────
# Always render fresh from the image-baked template — never use a stale
# file_follow.conf that may be sitting on the state volume.
if [ ! -f "${CONFIG_TEMPLATE}" ]; then
    echo "[entrypoint] ERROR: template not found at ${CONFIG_TEMPLATE}" >&2
    exit 1
fi

cp "${CONFIG_TEMPLATE}" "${CONFIG_FILE}"

# Ingest secret
if [ -n "${GRAVWELL_INGEST_SECRET}" ]; then
    sed -i "s|GRAVWELL_INGEST_SECRET_PLACEHOLDER|${GRAVWELL_INGEST_SECRET}|g" \
        "${CONFIG_FILE}"
fi

# Cleartext targets (comma-separated → multiple lines)
if [ -n "${GRAVWELL_CLEARTEXT_TARGETS}" ]; then
    sed -i '/Cleartext-Backend-Target/d' "${CONFIG_FILE}"
    echo "${GRAVWELL_CLEARTEXT_TARGETS}" | tr ',' '\n' | while read -r t; do
        t=$(echo "${t}" | sed 's/^[[:space:]]*//;s/[[:space:]]*$//')
        [ -n "${t}" ] && \
            sed -i "/^\[Global\]/a Cleartext-Backend-Target=${t}" "${CONFIG_FILE}"
    done
fi

# Encrypted targets (comma-separated → multiple lines)
if [ -n "${GRAVWELL_ENCRYPTED_TARGETS}" ]; then
    sed -i '/Encrypted-Backend-Target/d' "${CONFIG_FILE}"
    echo "${GRAVWELL_ENCRYPTED_TARGETS}" | tr ',' '\n' | while read -r t; do
        t=$(echo "${t}" | sed 's/^[[:space:]]*//;s/[[:space:]]*$//')
        [ -n "${t}" ] && \
            sed -i "/^\[Global\]/a Encrypted-Backend-Target=${t}" "${CONFIG_FILE}"
    done
fi

# TLS skip-verify (accept either name for convenience)
if [ "${GRAVWELL_INSECURE_SKIP_TLS_VERIFY}" = "true" ] || \
   [ "${INSECURE_SKIP_TLS_VERIFY}" = "true" ]; then
    sed -i "s|.*Insecure-Skip-TLS-Verify=.*|Insecure-Skip-TLS-Verify=true|g" \
        "${CONFIG_FILE}"
fi

# Log level
if [ -n "${GRAVWELL_LOG_LEVEL}" ]; then
    sed -i "s|.*Log-Level=.*|Log-Level=${GRAVWELL_LOG_LEVEL}|g" \
        "${CONFIG_FILE}"
fi

# ── Stable ingester UUID ─────────────────────────────────────────────────────
# If GRAVWELL_INGESTER_UUID is set, use it.  Otherwise, check for a persisted
# UUID on the state volume.  If neither exists, generate one and persist it so
# the ingester keeps the same identity across redeploys.
UUID_FILE="/opt/gravwell/etc/.ingester-uuid"
INGESTER_UUID="${GRAVWELL_INGESTER_UUID:-}"
if [ -z "${INGESTER_UUID}" ] && [ -f "${UUID_FILE}" ]; then
    INGESTER_UUID=$(cat "${UUID_FILE}")
fi
if [ -z "${INGESTER_UUID}" ]; then
    # Generate a v4 UUID using /proc/sys/kernel/random/uuid (Linux)
    # or fall back to a simple cat /dev/urandom construction.
    if [ -f /proc/sys/kernel/random/uuid ]; then
        INGESTER_UUID=$(cat /proc/sys/kernel/random/uuid)
    else
        INGESTER_UUID=$(od -x /dev/urandom | head -1 | awk '{OFS="-"; print $2$3,$4,$5,$6,$7$8$9}')
    fi
    echo "${INGESTER_UUID}" > "${UUID_FILE}"
    echo "[entrypoint] generated new Ingester-UUID: ${INGESTER_UUID}"
fi
sed -i "s|.*#Ingester-UUID=.*|	Ingester-UUID=${INGESTER_UUID}|" "${CONFIG_FILE}"

# ── Ingester label ────────────────────────────────────────────────────────────
if [ -n "${GRAVWELL_INGESTER_LABEL}" ]; then
    sed -i "s|.*#Label=.*|	Label=${GRAVWELL_INGESTER_LABEL}|" "${CONFIG_FILE}"
fi

# ── Source override ───────────────────────────────────────────────────────────
# Pins the SRC field on every entry to a fixed IP regardless of which
# ephemeral bridge/connection address the ingester actually connects from.
if [ -n "${GRAVWELL_SOURCE_OVERRIDE}" ]; then
    sed -i "s|.*#Source-Override=.*|	Source-Override=${GRAVWELL_SOURCE_OVERRIDE}|" "${CONFIG_FILE}"
fi

# Debug dump
if [ "${DEBUG}" = "true" ]; then
    echo "=== Rendered base config ==="
    cat "${CONFIG_FILE}"
    echo "============================"
fi

# ── Hand off to docker-log-watcher (supervises gravwell_file_follow) ──────────
exec /usr/local/bin/docker-log-watcher
