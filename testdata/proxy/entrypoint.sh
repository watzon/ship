#!/bin/sh
set -eu
ssh-keygen -A
/usr/local/bin/dockerd-entrypoint.sh dockerd --host=unix:///var/run/docker.sock > /tmp/dockerd.log 2>&1 &
docker_pid=$!
trap 'kill "$docker_pid" 2>/dev/null || true' EXIT INT TERM
/usr/sbin/sshd -D -e &
wait "$!"
