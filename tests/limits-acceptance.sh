#!/usr/bin/env bash
# Offline limit/accounting acceptance suite. Never contacts a production panel,
# changes a node configuration, or sends traffic through user accounts.
set -Eeuo pipefail

ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT"
GO_BIN="${GO_BIN:-go}"
TAGS="with_quic with_utls with_wireguard with_clash_api"
REPORT_DIR="${REPORT_DIR:-${TMPDIR:-/tmp}/xboard-limits-acceptance-$(date -u +%Y%m%d-%H%M%S)}"
mkdir -p "$REPORT_DIR"
LOG="$REPORT_DIR/results.txt"
: > "$LOG"
failed=0

run_suite() {
  local label="$1"
  shift
  echo "===== $label =====" | tee -a "$LOG"
  if "$GO_BIN" test -count=1 -timeout=6m -tags "$TAGS" "$@" 2>&1 | tee -a "$LOG"; then
    echo "PASS: $label" | tee -a "$LOG"
  else
    echo "FAIL: $label" | tee -a "$LOG"
    failed=1
  fi
}

run_suite "User speed limiter and limit configuration" ./internal/limiter
run_suite "Sing-box TCP/UDP speed and device limits" ./internal/kernel/singbox -run 'Test(LimitedTCPWriteAcrossBursts|SharedLimiterAcrossTCPConnections|UDPUnwrapAccountingDirections|UDPPacketBudgetLargerThanBurst|UDPPacketBudgetCancellation|UDPPacketBudgetInvalidBurst|LimitedUDPDoesNotAdvertiseReplaceableIO|GlobalDeviceAliasesCountOnce|GlobalDeviceAliasesDoNotConsumeExtraSlots|RemovedUserGlobalDevicesAreDiscarded|TrafficAccountingSurvivesRetiredTrackerArchive)'
run_suite "Xray device admission, concurrency and traffic tracking" ./internal/kernel/xray -run 'Test(LimitDispatcher_|DispatcherConcurrentAdmission|DispatcherGlobalDeviceUnion|XrayGetUserTrafficUsesBuiltInStatsAndDispatcherState|XrayUpdateBandwidthLimits)'
run_suite "Device state and traffic report reliability" ./internal/tracker ./internal/reportjournal ./internal/service ./internal/panel -run 'Test(DeviceRecoveryAfterRestart|IdempotentReportRetriesSameDiskBatchIDAfterRestart|IdempotentStorageFailureRestoresCountersWithoutSending|JournalDeviceSnapshotHasIndependentReport|DeviceReportCounts|LegacyReportQuarantinesAmbiguousFailureWithoutReplay|LegacyReportStorageFailurePreventsSend|JournalRetryPreservesBatchIdentityAcrossRestart|JournalBacklogDrainsAtBoundedRate|ReportBatchIDIsOptInAndRawBytesUnchanged|IdempotentReportDoesNotAcceptLegacyOrMismatchedReceipt)'

echo "===== Summary =====" | tee -a "$LOG"
grep -E '^(PASS|FAIL): ' "$LOG" | tail -n 4
echo "Report: $LOG"
echo "NOTE: This suite uses isolated fixtures only. It does NOT prove production panel quota exhaustion, multi-device public-IP behavior, or measured live throughput." | tee -a "$LOG"
exit "$failed"
