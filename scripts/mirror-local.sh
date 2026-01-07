#!/bin/bash
# Mirror OpenShift release using fastsync
# Usage: ./mirror-local.sh <version>

set -e

if [ -z "$1" ]; then
    echo "Usage: $0 <version>"
    exit 1
fi

VERSION="$1"
LOCAL_REGISTRY="registry.gw.lo"
LOCAL_REPO="openshift/release"
PULL_SECRET="/tmp/pullsecret.json"
LOCAL_RELEASE="${LOCAL_REGISTRY}/${LOCAL_REPO}:${VERSION}-x86_64"
CACHE_DIR="/var/lib/openshift-cache"

echo "=== FastSync OpenShift ${VERSION} ==="
echo "Started: $(date)"
echo ""

# Step 1: Clear old caches
echo "[1/5] Clearing old caches..."
rm -rf /root/.cache/agent/
rm -rf /tmp/agent-install-*
echo "  - Cleared /root/.cache/agent/"
echo "  - Cleared /tmp/agent-install-*"

# Step 2: Clear ISO on pve.gw.lo if accessible
echo "[2/5] Clearing remote ISO cache on pve.gw.lo..."
ssh root@pve.gw.lo "rm -f /var/lib/vz/template/iso/coreos-x86_64.iso /var/lib/vz/template/iso/agent.x86_64.iso" 2>/dev/null && echo "  - Cleared pve.gw.lo ISO cache" || echo "  - pve.gw.lo not accessible (skipped)"

# Step 3: Run fastsync
echo "[3/5] Mirroring release with fastsync..."
/tmp/fastsync --version "${VERSION}" --dest-registry "${LOCAL_REGISTRY}" --pull-secret "${PULL_SECRET}" --insecure

# Step 4: Extract openshift-install for all platforms
echo "[4/5] Extracting openshift-install for all platforms..."
mkdir -p "${CACHE_DIR}"
MIRROR_URL="https://mirror.openshift.com/pub/openshift-v4/clients/ocp/${VERSION}"

# Linux (from release image - most accurate)
echo "  - Extracting Linux binary from release..."
oc adm release extract --command=openshift-install --to="${CACHE_DIR}" --insecure "${LOCAL_RELEASE}"
mv "${CACHE_DIR}/openshift-install" "${CACHE_DIR}/openshift-install-${VERSION}"
chmod +x "${CACHE_DIR}/openshift-install-${VERSION}"
sha256sum "${CACHE_DIR}/openshift-install-${VERSION}" | cut -d" " -f1 > "${CACHE_DIR}/openshift-install-${VERSION}.sha256"
echo "    Saved: openshift-install-${VERSION}"

# macOS (download from mirror)
echo "  - Downloading macOS binary..."
curl -sL "${MIRROR_URL}/openshift-install-mac-amd64-${VERSION}.tar.gz" | tar xz -C "${CACHE_DIR}"
mv "${CACHE_DIR}/openshift-install" "${CACHE_DIR}/openshift-install-${VERSION}-mac"
chmod +x "${CACHE_DIR}/openshift-install-${VERSION}-mac"
sha256sum "${CACHE_DIR}/openshift-install-${VERSION}-mac" | cut -d" " -f1 > "${CACHE_DIR}/openshift-install-${VERSION}-mac.sha256"
echo "    Saved: openshift-install-${VERSION}-mac"

# Windows (download from mirror)
echo "  - Downloading Windows binary..."
curl -sL "${MIRROR_URL}/openshift-install-windows-${VERSION}.zip" -o /tmp/oi-win.zip
unzip -q -o /tmp/oi-win.zip -d "${CACHE_DIR}"
mv "${CACHE_DIR}/openshift-install.exe" "${CACHE_DIR}/openshift-install-${VERSION}.exe"
sha256sum "${CACHE_DIR}/openshift-install-${VERSION}.exe" | cut -d" " -f1 > "${CACHE_DIR}/openshift-install-${VERSION}.exe.sha256"
rm /tmp/oi-win.zip
echo "    Saved: openshift-install-${VERSION}.exe"

# Also install Linux binary to /usr/local/bin for local use
cp "${CACHE_DIR}/openshift-install-${VERSION}" /usr/local/bin/openshift-install
chmod +x /usr/local/bin/openshift-install

# Step 5: Show cached binaries
echo "[5/5] Cached binaries:"
ls -la "${CACHE_DIR}/openshift-install-${VERSION}"*

echo ""
echo "=== Complete ==="
echo "Finished: $(date)"
echo "Release: ${LOCAL_RELEASE}"
