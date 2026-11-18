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
echo 'Installer safety checks passed'
