#!/usr/bin/env bash
# Builds the service for linux/amd64 and ships the binary plus a systemd
# --user unit to the target host. config/config.yaml and .env are never
# touched by this script — they hold host-specific secrets (including the
# YAZIO account password) and are managed separately on the server.
#
# Usage:
#   MIRANDA_DEPLOY_HOST=archer@192.168.1.50 ./scripts/deploy.sh
set -euo pipefail
cd "$(dirname "$0")/.."

remote_host="${MIRANDA_DEPLOY_HOST:?set MIRANDA_DEPLOY_HOST, e.g. user@host}"
remote_dir="miranda-yazio"
service_name="miranda-yazio"
binary_name="miranda-yazio"    # keep in sync with Makefile's BINARY
healthz_port="8790"            # keep in sync with config/config.yaml's http_addr
build_out="dist/${binary_name}-linux-amd64"
unit_file="$(mktemp)"
trap 'rm -f "$unit_file"' EXIT

echo "==> Building $build_out (GOOS=linux GOARCH=amd64, CGO_ENABLED=0)"
mkdir -p dist
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -o "$build_out" ./cmd/miranda-yazio

cat >"$unit_file" <<EOF
[Unit]
Description=${service_name} MCP server
After=network-online.target
Wants=network-online.target

[Service]
Type=simple
WorkingDirectory=%h/${remote_dir}
ExecStart=%h/${remote_dir}/${service_name}
Restart=on-failure
RestartSec=2

[Install]
WantedBy=default.target
EOF

echo "==> Uploading binary and systemd unit to ${remote_host}:~/${remote_dir}"
ssh "$remote_host" "mkdir -p ~/${remote_dir} ~/.config/systemd/user"
scp -q "$build_out" "${remote_host}:~/${remote_dir}/${service_name}.new"
scp -q "$unit_file" "${remote_host}:~/.config/systemd/user/${service_name}.service"

echo "==> Installing and restarting on ${remote_host}"
ssh "$remote_host" bash -s -- "$remote_dir" "$service_name" "$healthz_port" <<'REMOTE'
set -euo pipefail
remote_dir="$1"
service_name="$2"
healthz_port="$3"

cd ~/"$remote_dir"

chmod +x ~/"$remote_dir"/"$service_name".new
mv ~/"$remote_dir"/"$service_name".new ~/"$remote_dir"/"$service_name"

systemctl --user daemon-reload
systemctl --user enable --now "$service_name" >/dev/null
systemctl --user restart "$service_name"

if [ "$(loginctl show-user "$(whoami)" --property=Linger --value 2>/dev/null)" != "yes" ]; then
  echo "WARNING: lingering is not enabled — service will stop when SSH session ends." >&2
  echo "  Run once: loginctl enable-linger $(whoami)" >&2
fi

echo "--- systemctl --user status $service_name ---"
systemctl --user --no-pager -l status "$service_name" || true

echo "--- health check ---"
for i in 1 2 3 4 5; do
  if curl -fsS -m 2 "http://localhost:${healthz_port}/healthz"; then
    echo
    exit 0
  fi
  sleep 1
done
echo "healthz check failed after restart" >&2
exit 1
REMOTE

echo "==> Deploy complete"
