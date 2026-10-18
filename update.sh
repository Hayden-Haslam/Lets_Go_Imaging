#!/usr/bin/env bash
# ==============================================================================
# Let's Go Imaging - Root Application Update Script (AlmaLinux 10 / RHEL 10)
# Usage:
#   ./update.sh                  (SSH / CLI: git pull, build/update, restart services)
#   ./update.sh --no-restart     (Used by Web UI step 1: pull & build before restart)
#   ./update.sh --restart-only   (Used by Web UI step 2: restart systemd services)
# ==============================================================================
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
cd "$SCRIPT_DIR"

MODE="${1:-full}"

restart_services() {
  if command -v systemctl >/dev/null 2>&1; then
    echo "==> Restarting Let's Go Imaging systemd services..."
    if systemctl list-unit-files | grep -q "^lets-go-imaging.service"; then
      systemctl restart lets-go-imaging.service || true
      echo "    [OK] Restarted lets-go-imaging.service"
    fi
    if systemctl list-unit-files | grep -q "^lets-go-imaging-web.service"; then
      systemctl restart lets-go-imaging-web.service || true
      echo "    [OK] Restarted lets-go-imaging-web.service"
    fi
  else
    echo "==> systemctl not found; please restart the Go and Ruby processes manually."
  fi
}

if [[ "$MODE" == "--restart-only" ]]; then
  restart_services
  exit 0
fi

echo "========================================================"
echo "  Let's Go Imaging - Updating Application"
echo "  Directory: $SCRIPT_DIR"
echo "========================================================"

# 1. Pull latest changes from Git repository (if git is available & .git exists)
if [[ -d ".git" ]] && command -v git >/dev/null 2>&1; then
  echo "==> [1/3] Pulling latest changes from Git..."
  git pull --ff-only
else
  echo "==> [1/3] Skipping git pull (not a git checkout or git not installed)."
fi

# 2. Go Backend: Compile from source if Go is installed, or use pre-built binary
echo "==> [2/3] Updating Go iPXE/Imaging Server (ipxe-server)..."
cd "$SCRIPT_DIR/ipxe-server"
if command -v go >/dev/null 2>&1; then
  echo "    Go compiler detected ($(go version)). Building static binary..."
  CGO_ENABLED=0 go build -o lets-go-imaging.new .
  mv -f lets-go-imaging.new lets-go-imaging
  chmod +x lets-go-imaging
  echo "    [OK] Built ipxe-server/lets-go-imaging"
elif [[ -f "lets-go-imaging" ]]; then
  chmod +x lets-go-imaging
  echo "    [OK] Using pre-built binary ipxe-server/lets-go-imaging (Go compiler not required)"
else
  echo "    [ERROR] Neither 'go' compiler nor pre-built 'ipxe-server/lets-go-imaging' binary found!"
  exit 1
fi

# 3. Ruby Web UI: Verify/install pure-Ruby gems
echo "==> [3/3] Checking Ruby Sinatra Web UI dependencies (web-ui)..."
cd "$SCRIPT_DIR/web-ui"
if command -v bundle >/dev/null 2>&1; then
  bundle check >/dev/null 2>&1 || bundle install
  echo "    [OK] Ruby gems verified"
else
  echo "    [WARN] 'bundle' not found; skipping gem check"
fi

cd "$SCRIPT_DIR"

if [[ "$MODE" != "--no-restart" ]]; then
  restart_services
fi

echo "========================================================"
echo "  Update Complete!"
echo "========================================================"
