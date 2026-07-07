#!/usr/bin/env bash
#
# Deploy website-errors-notifier on a Fedora server.
# Copy the whole project dir to the server, then run as root from inside it:
#     sudo ./deploy.sh
#
# Builds the Go binary, installs it + .env to /opt/error-notify under a
# dedicated system user, installs the systemd unit, and (re)starts it.
set -euo pipefail

APP=website-errors-notifier
APP_DIR=/opt/$APP
SVC=$APP.service
RUN_USER=webnotifier
SRC_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

[[ $EUID -eq 0 ]] || { echo "ERROR: run as root (sudo ./deploy.sh)" >&2; exit 1; }

# 1. Go toolchain
if ! command -v go >/dev/null 2>&1; then
  echo ">> installing golang..."
  dnf install -y golang
fi

# 2. build (static, stdlib-only — no network needed)
echo ">> building $APP..."
cd "$SRC_DIR"
CGO_ENABLED=0 go build -trimpath -o "$APP" .

# 3. dedicated system user
if ! id -u "$RUN_USER" >/dev/null 2>&1; then
  echo ">> creating system user $RUN_USER..."
  useradd --system --no-create-home --shell /sbin/nologin "$RUN_USER"
fi

# 4. install binary + .env
echo ">> installing to $APP_DIR..."
install -d -o "$RUN_USER" -g "$RUN_USER" -m 0750 "$APP_DIR"
install -o "$RUN_USER" -g "$RUN_USER" -m 0755 "$APP" "$APP_DIR/$APP"
if [[ -f "$SRC_DIR/.env" ]]; then
  install -o "$RUN_USER" -g "$RUN_USER" -m 0600 "$SRC_DIR/.env" "$APP_DIR/.env"
else
  echo "!! WARNING: .env not found in $SRC_DIR — copy .env.example to .env and fill secrets, then re-run." >&2
fi

# 5. SELinux context (Fedora enforces by default)
if command -v restorecon >/dev/null 2>&1; then
  restorecon -RF "$APP_DIR" 2>/dev/null || true
fi

# 6. systemd unit
echo ">> installing $SVC..."
install -m 0644 "$SRC_DIR/$SVC" "/etc/systemd/system/$SVC"
restorecon -F "/etc/systemd/system/$SVC" 2>/dev/null || true

# 7. enable + (re)start
systemctl daemon-reload
systemctl enable "$SVC"
systemctl restart "$SVC"

echo ">> done. status:"
systemctl --no-pager --full status "$SVC" || true
echo ">> logs: journalctl -u $SVC -f"
