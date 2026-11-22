#!/usr/bin/env bash
# Offline limit/accounting acceptance suite. Never contacts a production panel,
# changes a node configuration, or sends traffic through user accounts.
set -Eeuo pipefail

# When downloaded as a standalone script, prefer the source directory that
# invoked it. In CI, resolve the repository root relative to this file.
if [[ -f ./go.mod ]]; then
  ROOT="$(pwd)"
else
  ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
fi
if [[ ! -f "$ROOT/go.mod" ]]; then
  echo "ERROR: run from the checked-out Xboard-Node source directory (go.mod missing)" >&2
  exit 2
fi
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

run_suite "Atomic shared-machine device reservation and concurrent admission" ./internal/devicegate
run_suite "User speed limiter and limit configuration" ./internal/limiter
run_suite "Sing-box TCP/UDP speed, admission and device change notification" ./internal/kernel/singbox -run 'Test(DeviceEventsOnSingBoxConnectionAdmitAndClose|NoDeviceEventOnRejectedSingBoxAdmission|SharedMachineAdmissionRejectsThirdCrossProtocolIP|LimitedTCPWriteAcrossBursts|SharedLimiterAcrossTCPConnections|UDPUnwrapAccountingDirections|UDPPacketBudgetLargerThanBurst|UDPPacketBudgetCancellation|UDPPacketBudgetInvalidBurst|LimitedUDPDoesNotAdvertiseReplaceableIO|GlobalDeviceAliasesCountOnce|GlobalDeviceAliasesDoNotConsumeExtraSlots|RemovedUserGlobalDevicesAreDiscarded|TrafficAccountingSurvivesRetiredTrackerArchive)'
run_suite "Xray device admission, notification, concurrency and traffic tracking" ./internal/kernel/xray -run 'Test(DeviceEventsOnXrayAdmitAndClose|XraySharedAdmissionUsesCanonicalUserID|LimitDispatcher_|DispatcherConcurrentAdmission|DispatcherGlobalDeviceUnion|XrayGetUserTrafficUsesBuiltInStatsAndDispatcherState|XrayUpdateBandwidthLimits)'
# Run full tracker/journal packages; a narrow name filter previously ran zero tests.
run_suite "Online devices, IPv4/IPv6 normalization, traffic deltas and offline recovery" ./internal/tracker
run_suite "Traffic journal, persistence and crash recovery" ./internal/reportjournal
run_suite "Panel device reports, event coalescing and idempotent traffic delivery" ./internal/service ./internal/panel -run '^Test(TwoNodesLostBillingReceiptsRecoverWithoutDoubleCharge|DeviceChangeSignalCoalescesWithoutBlocking|DeviceRecoveryAfterRestart|IdempotentReportRetriesSameDiskBatchIDAfterRestart|IdempotentStorageFailureRestoresCountersWithoutSending|JournalDeviceSnapshotHasIndependentReport|DeviceReportCounts|LegacyReportQuarantinesAmbiguousFailureWithoutReplay|LegacyReportStorageFailurePreventsSend|JournalRetryPreservesBatchIdentityAcrossRestart|JournalBacklogDrainsAtBoundedRate|ReportBatchIDIsOptInAndRawBytesUnchanged|IdempotentReportDoesNotAcceptLegacyOrMismatchedReceipt)$'
# Loopback encrypted client roundtrips validate protocol and byte accounting.
run_suite "Encrypted loopback client traffic and per-user accounting" ./internal/kernel/singbox -run '^TestProtocolClientRoundTrip$'

echo "===== Summary =====" | tee -a "$LOG"
grep -E '^(PASS|FAIL): ' "$LOG" | tail -n 8
echo "Report: $LOG"
echo "NOTE: This suite uses isolated fixtures only. It does NOT prove production panel quota exhaustion, multi-device public-IP behavior, or measured live throughput." | tee -a "$LOG"
exit "$failed"
