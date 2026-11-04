#!/bin/sh
# ProxFin container entrypoint.
#
# ProxFin is based on Threadfin, whose image kept its settings in
# /home/threadfin/conf. A container switched to ProxFin that still mounts
# its volume there keeps using it, and THREADFIN_* variables from an
# existing compose file still apply.
set -e

conf="${PROXFIN_CONF}"
if [ -n "$(ls -A /home/threadfin/conf 2>/dev/null)" ] && [ -z "$(ls -A "${conf}" 2>/dev/null)" ]; then
	conf=/home/threadfin/conf
	echo "Using the existing Threadfin settings in ${conf}"
fi

# exec: ProxFin runs as PID 1 and gets the stop signal itself, so it can
# stop its FFmpeg/VLC processes before the container ends.
exec "${PROXFIN_BIN}/proxfin" \
	-port="${THREADFIN_PORT:-$PROXFIN_PORT}" \
	-bind="${THREADFIN_BIND_IP_ADDRESS:-$PROXFIN_BIND_IP_ADDRESS}" \
	-config="${THREADFIN_CONF:-$conf}" \
	-debug="${THREADFIN_DEBUG:-$PROXFIN_DEBUG}" \
	"$@"
