#!/bin/sh
set -eu
# New Fly Volumes are initially root-owned. Drop privileges before serving.
umask 077
if [ "$(id -u)" = 0 ]; then
  mkdir -p /data
  chown 10001:10001 /data
  chmod 700 /data
  exec setpriv --reuid=10001 --regid=10001 --init-groups /usr/local/bin/sprite-cron "$@"
fi
exec /usr/local/bin/sprite-cron "$@"
