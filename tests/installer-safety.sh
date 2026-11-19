#!/usr/bin/env bash
set -Eeuo pipefail
source "$(dirname "${BASH_SOURCE[0]}")/../install.sh"
TMP_DIR=$(mktemp -d)
artifact=xboard-node-linux-amd64
printf 'test binary\n' > "$TMP_DIR/bin"
hash=$(sha256sum "$TMP_DIR/bin")
hash=${hash%% *}
printf '%s  %s\n' "$hash" "$artifact" > "$TMP_DIR/SHA256SUMS"
verify_release_artifact "$artifact" "$TMP_DIR/bin"
printf 'corrupted\n' >> "$TMP_DIR/bin"
if verify_release_artifact "$artifact" "$TMP_DIR/bin"; then
    echo 'corrupted binary accepted' >&2
    exit 1
fi
printf '%s  %s\n%s  %s\n' "$hash" "$artifact" "$hash" "$artifact" > "$TMP_DIR/SHA256SUMS"
if verify_release_artifact "$artifact" "$TMP_DIR/bin"; then
    echo 'ambiguous checksum accepted' >&2
    exit 1
fi
if verify_release_artifact xbctl-linux-amd64 "$TMP_DIR/bin"; then
    echo 'missing checksum accepted' >&2
    exit 1
fi

# Exercise the actual error trap without a real systemd mutation.
marker="$TMP_DIR/rollback-called"
for state in incomplete ready_not_mutated mutated; do
    (
        # Parent owns the scratch directory.
        trap - EXIT
        cleanup_tmp() { :; }
        rollback_install() { touch "$marker"; }
        BACKUP_PATH="$TMP_DIR"
        BACKUP_READY=0
        MUTATION_STARTED=0
        if [[ "$state" != incomplete ]]; then BACKUP_READY=1; fi
        if [[ "$state" == mutated ]]; then MUTATION_STARTED=1; fi
        false
    ) &
    child=$!
    if wait "$child"; then
        echo 'failure trap returned success' >&2
        exit 1
    fi
    if [[ "$state" == mutated ]]; then
        test -f "$marker"
    else
        test ! -e "$marker"
    fi
done
# Local files are allowed only through explicit operator options.
(
    cd "$TMP_DIR"
    touch xboard-node xboard-node-linux-amd64 xbctl
    ARCH=amd64
    BINARY_SOURCE=""
    test -z "$(select_binary_source)"
    BINARY_SOURCE=./xboard-node
    test "$(select_binary_source)" = "./xboard-node"
)
for v in latest v1.0.0 v1.0.0-rc.5; do
    RELEASE_VERSION="$v"
    validate_release_version
done
for v in '../bad' 'v1.0.0/other' ''; do
    RELEASE_VERSION="$v"
    if validate_release_version; then
        echo 'invalid release accepted' >&2
        exit 1
    fi
done
# Use a temporary lock, never touch the actual host installer lock.
(
    trap - EXIT
    INSTALL_LOCK="$TMP_DIR/test.lock"
    acquire_install_lock
    if (exec 9>&-; acquire_install_lock); then
        echo 'concurrent installer accepted' >&2
        exit 1
    fi
)
(
    trap - EXIT
    INSTALL_LOCK="$TMP_DIR/test.lock"
    acquire_install_lock
)
# A damaged backup must not stop or overwrite the running service.
(
    trap - EXIT
    BACKUP_PATH="$TMP_DIR/damaged-backup"
    mkdir "$BACKUP_PATH"
    printf original > "$BACKUP_PATH/xboard-node"
    (cd "$BACKUP_PATH" && sha256sum xboard-node > SHA256SUMS)
    printf damaged > "$BACKUP_PATH/xboard-node"
    BACKUP_READY=1
    systemctl() { touch "$TMP_DIR/unexpected-systemctl"; return 0; }
    if rollback_install; then
        echo 'damaged rollback accepted' >&2
        exit 1
    fi
    test ! -e "$TMP_DIR/unexpected-systemctl"
)
# Simulate healthy service, startup crash, and recovered restart.
# Counts use files because command substitutions run in subshells.
(
    trap - EXIT
    HEALTH_ENABLED=0
    sleep() { :; }
    service_identity() { printf '42:0\n'; }
    wait_for_health
    counter="$TMP_DIR/health-counter"
    printf 0 > "$counter"
    service_identity() {
        local n
        n=$(cat "$counter")
        printf '%s' "$((n+1))" > "$counter"
        if [ "$n" -lt 3 ]; then printf '42:0\n'; else printf '43:1\n'; fi
    }
    if wait_for_health; then
        echo 'restarted service accepted' >&2
        exit 1
    fi
    service_identity() { return 1; }
    if wait_for_health; then
        echo 'inactive service accepted' >&2
        exit 1
    fi
    HEALTH_ENABLED=1
    service_identity() { printf '42:0\n'; }
    curl() { return 1; }
    if wait_for_health; then
        echo 'unhealthy endpoint accepted' >&2
        exit 1
    fi
    curl() { return 0; }
    wait_for_health
)
echo 'Installer safety checks passed'
