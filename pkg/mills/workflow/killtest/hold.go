package killtest

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/crb2nu/loom/pkg/mills/workflow"
)

// CanaryHoldObservation identifies the exact side-effect-free process that
// makes the workflow canary safe to fault. A Ready pod alone does not prove
// that the agent has entered this bounded hold.
type CanaryHoldObservation struct {
	PodName              string    `json:"pod_name"`
	PID                  int       `json:"pid"`
	StartTimeTicks       uint64    `json:"start_time_ticks"`
	DriverPID            int       `json:"driver_pid"`
	DriverStartTimeTicks uint64    `json:"driver_start_time_ticks"`
	Seconds              int       `json:"seconds"`
	ObservedAt           time.Time `json:"observed_at"`
}

// CanaryProcessSample records both the original canary process identities and
// the complete live canary-process inventory after CRASH B. Reading the known
// PIDs independently of cmdline preserves zombie evidence because Linux
// exposes an empty /proc/<pid>/cmdline for zombies. ObservedAt is the
// conservative probe-start time; CompletedAt proves when the full response was
// available to authorize a delete.
type CanaryProcessSample struct {
	PodName              string    `json:"pod_name"`
	ObservedAt           time.Time `json:"observed_at"`
	CompletedAt          time.Time `json:"completed_at"`
	HoldPID              int       `json:"hold_pid"`
	HoldStartTimeTicks   uint64    `json:"hold_start_time_ticks"`
	HoldState            string    `json:"hold_state"`
	DriverPID            int       `json:"driver_pid"`
	DriverStartTimeTicks uint64    `json:"driver_start_time_ticks"`
	DriverState          string    `json:"driver_state"`
	LiveHoldPIDs         []int     `json:"live_hold_pids"`
	LiveDriverPIDs       []int     `json:"live_driver_pids"`
	ZombiePIDs           []int     `json:"zombie_pids"`
}

// procSnapshotBash reads stat/status/stat around a process identity. The
// second stat read proves that PID reuse did not splice status from a different
// process into the observation. State may legitimately change between reads,
// and so may PPid — a process whose parent dies is reparented to init — so
// starttime is the only value required to stay stable; see ppid_reparent_ok.
const procSnapshotBash = `
read_proc_stat() {
	proc_stat_state=
	proc_stat_ppid=
	proc_stat_start=
	local stat_line remainder
	IFS= read -r stat_line < "$1/stat" 2>/dev/null || return 1
	remainder=${stat_line##*) }
	[ "$remainder" != "$stat_line" ] || return 1
	local fields=()
	read -r -a fields <<< "$remainder"
	[ "${#fields[@]}" -ge 20 ] || return 1
	proc_stat_state=${fields[0]}
	proc_stat_ppid=${fields[1]}
	proc_stat_start=${fields[19]}
	case "$proc_stat_state" in R|S|D|Z|T|t|X|x|K|W|P|I) ;; *) return 1 ;; esac
	case "$proc_stat_ppid" in ''|*[!0-9]*) return 1 ;; esac
	case "$proc_stat_start" in ''|*[!0-9]*) return 1 ;; esac
	[ "$proc_stat_start" -gt 0 ] || return 1
}

ppid_reparent_ok() {
	[ "$2" = "$1" ] || [ "$2" = 1 ]
}

read_process_snapshot() {
	snapshot_state=
	snapshot_ppid=
	snapshot_start=
	read_proc_stat "$1" || return 1
	local first_ppid=$proc_stat_ppid
	local first_start=$proc_stat_start
	local status_state= status_ppid=
	while IFS=$' \t' read -r key value _; do
		case "$key" in
			State:) status_state=$value ;;
			PPid:) status_ppid=$value ;;
		esac
	done < "$1/status" 2>/dev/null || return 1
	case "$status_state" in R|S|D|Z|T|t|X|x|K|W|P|I) ;; *) return 1 ;; esac
	case "$status_ppid" in ''|*[!0-9]*) return 1 ;; esac
	read_proc_stat "$1" || return 1
	[ "$first_start" = "$proc_stat_start" ] || return 1
	# (PID,starttime) is the process identity and the bracketing stat reads
	# above prove it held across this whole snapshot, so PID reuse is already
	# excluded. PPID is mutable STATE, not identity: when a process's parent
	# dies the kernel reparents it to init, which is exactly what CRASH B
	# causes mid-probe. Permit each later observation to be either the
	# original parent or 1 (orphaned); any other value is cross-process
	# confusion and still fails closed. Since first_ppid is the earliest
	# read, this also rejects the impossible 1 -> non-1 "un-orphaning".
	ppid_reparent_ok "$first_ppid" "$status_ppid" || return 1
	ppid_reparent_ok "$first_ppid" "$proc_stat_ppid" || return 1
	snapshot_state=$proc_stat_state
	snapshot_ppid=$proc_stat_ppid
	snapshot_start=$proc_stat_start
}
`

// canaryHoldProbeScript inspects exact argv boundaries and binds both the hold
// and its completion wrapper to Linux (PID,starttime) identities. $1 is the
// expected duration; $2 is a test-only proc root.
const canaryHoldProbeScript = `set -u
target_seconds=$1
proc_root=${2:-/proc}
` + procSnapshotBash + `
hold_count=0
hold_pid=
hold_ppid=
hold_start=
driver_count=0
driver_pid=
driver_start=

for proc in "$proc_root"/[0-9]*; do
	[ -d "$proc" ] || continue
	pid=${proc##*/}
	read_process_snapshot "$proc" || continue
	[ "$snapshot_state" != Z ] || continue
	first_start=$snapshot_start
	first_ppid=$snapshot_ppid
	argv=()
	mapfile -d '' -t argv < "$proc/cmdline" 2>/dev/null || continue
	read_process_snapshot "$proc" || continue
	[ "$snapshot_start" = "$first_start" ] || continue
	ppid_reparent_ok "$first_ppid" "$snapshot_ppid" || continue
	[ "$snapshot_state" != Z ] || continue

	if [ "${#argv[@]}" -eq 2 ] &&
		[ "${argv[0]##*/}" = sleep ] &&
		[ "${argv[1]}" = "$target_seconds" ]; then
		hold_count=$((hold_count + 1))
		hold_pid=$pid
		hold_ppid=$snapshot_ppid
		hold_start=$snapshot_start
		continue
	fi

	[ "${#argv[@]}" -eq 3 ] || continue
	case "${argv[0]##*/}" in sh|bash|dash|ash) ;; *) continue ;; esac
	[ "${argv[1]}" = -c ] || continue
	[[ "${argv[2]}" == *'loom_agent_exit=$?;'* ]] || continue
	[[ "${argv[2]}" == *"; sleep ${target_seconds}; loom_hold_exit=\$?;"* ]] || continue
	driver_count=$((driver_count + 1))
	driver_pid=$pid
	driver_start=$snapshot_start
done

case "$hold_count" in
	0) printf 'NOT_READY\n' ;;
	1)
		if [ "$driver_count" -eq 1 ] && [ "$driver_pid" = "$hold_ppid" ] && [ "$hold_ppid" -gt 1 ]; then
			printf 'READY\t%s\t%s\t%s\t%s\t%s\n' \
				"$hold_pid" "$hold_start" "$target_seconds" "$driver_pid" "$driver_start"
		else
			printf 'DRIVER_MISMATCH\t%s\t%s\n' "$driver_count" "$hold_ppid"
		fi
		;;
	*) printf 'AMBIGUOUS\t%s\n' "$hold_count" ;;
esac
`

// canaryProcessProbeScript records the original identities, every exact live
// hold/wrapper PID, and every zombie PID in the container. Inputs are duration,
// hold PID/starttime, driver PID/starttime, and an optional test proc root.
// The optional 7th input is test-only: a comma-separated PID list the
// enumeration loop skips, deterministically forcing the transient inventory
// miss that recheck_original_live exists to heal. Skipping can only DROP
// candidates from the inventory, never admit them, so the seam cannot loosen
// the probe.
const canaryProcessProbeScript = `set -u
target_seconds=$1
expected_hold_pid=$2
expected_hold_start=$3
expected_driver_pid=$4
expected_driver_start=$5
proc_root=${6:-/proc}
test_skip_pids=${7:-}
` + procSnapshotBash + `
read_known_identity() {
	known_state=MISSING
	known_start=0
	[ -d "$1" ] || return 0
	if ! read_process_snapshot "$1"; then
		known_state=INVALID
		return 0
	fi
	known_state=$snapshot_state
	known_start=$snapshot_start
}

read_known_identity "$proc_root/$expected_hold_pid"
hold_state=$known_state
hold_start=$known_start
read_known_identity "$proc_root/$expected_driver_pid"
driver_state=$known_state
driver_start=$known_start

live_hold_pids=()
live_driver_pids=()
zombie_pids=()
for proc in "$proc_root"/[0-9]*; do
	[ -d "$proc" ] || continue
	pid=${proc##*/}
	case ",$test_skip_pids," in *",$pid,"*) continue ;; esac
	read_process_snapshot "$proc" || continue
	if [ "$snapshot_state" = Z ]; then
		zombie_pids+=("$pid")
		continue
	fi
	first_start=$snapshot_start
	first_ppid=$snapshot_ppid
	argv=()
	mapfile -d '' -t argv < "$proc/cmdline" 2>/dev/null || continue
	read_process_snapshot "$proc" || continue
	[ "$snapshot_start" = "$first_start" ] || continue
	ppid_reparent_ok "$first_ppid" "$snapshot_ppid" || continue
	if [ "$snapshot_state" = Z ]; then
		zombie_pids+=("$pid")
		continue
	fi

	if [ "${#argv[@]}" -eq 2 ] &&
		[ "${argv[0]##*/}" = sleep ] &&
		[ "${argv[1]}" = "$target_seconds" ]; then
		live_hold_pids+=("$pid")
		continue
	fi

	[ "${#argv[@]}" -eq 3 ] || continue
	case "${argv[0]##*/}" in sh|bash|dash|ash) ;; *) continue ;; esac
	[ "${argv[1]}" = -c ] || continue
	[[ "${argv[2]}" == *'loom_agent_exit=$?;'* ]] || continue
	[[ "${argv[2]}" == *"; sleep ${target_seconds}; loom_hold_exit=\$?;"* ]] || continue
	live_driver_pids+=("$pid")
done

# A process-tree transition (reaper exit, orphan reparenting) can land inside
# the enumeration loop's multi-read snapshot of an original PID and drop a
# provably-alive process from the inventory for exactly one sample, while the
# known-identity read above — taken at a different instant — still binds the
# same (PID,starttime) alive (S1c v5 run 1, sample 313: driver state=S,
# live=[]). Re-visit ONLY an absent original through the SAME full contract —
# snapshot, exact argv, snapshot — after the loop: the transition has settled
# by then, so a live original re-matches, while a dead, replaced, or
# mismatched process still fails every branch. The guard never fires when the
# inventory already holds any candidate (replacement/overlap detection is
# untouched) and never resurrects MISSING/INVALID or identity-drifted reads.
# The recheck additionally pins its own observed starttime to the expected
# original, so even a same-PID reuse between the known-identity read and this
# recheck fails closed — strictly stronger than the enumeration loop.
recheck_original_live() {
	local kind=$1 recheck_proc="$proc_root/$2" expected_start=$3
	[ -d "$recheck_proc" ] || return 1
	read_process_snapshot "$recheck_proc" || return 1
	[ "$snapshot_state" != Z ] || return 1
	[ "$snapshot_start" = "$expected_start" ] || return 1
	local recheck_start=$snapshot_start recheck_ppid=$snapshot_ppid
	argv=()
	mapfile -d '' -t argv < "$recheck_proc/cmdline" 2>/dev/null || return 1
	read_process_snapshot "$recheck_proc" || return 1
	[ "$snapshot_start" = "$recheck_start" ] || return 1
	ppid_reparent_ok "$recheck_ppid" "$snapshot_ppid" || return 1
	[ "$snapshot_state" != Z ] || return 1
	if [ "$kind" = hold ]; then
		[ "${#argv[@]}" -eq 2 ] &&
			[ "${argv[0]##*/}" = sleep ] &&
			[ "${argv[1]}" = "$target_seconds" ]
		return
	fi
	[ "${#argv[@]}" -eq 3 ] || return 1
	case "${argv[0]##*/}" in sh|bash|dash|ash) ;; *) return 1 ;; esac
	[ "${argv[1]}" = -c ] || return 1
	[[ "${argv[2]}" == *'loom_agent_exit=$?;'* ]] || return 1
	[[ "${argv[2]}" == *"; sleep ${target_seconds}; loom_hold_exit=\$?;"* ]]
}

if [ "${#live_hold_pids[@]}" -eq 0 ] &&
	[ "$hold_state" != MISSING ] && [ "$hold_state" != INVALID ] &&
	[ "$hold_start" = "$expected_hold_start" ] &&
	recheck_original_live hold "$expected_hold_pid" "$expected_hold_start"; then
	live_hold_pids+=("$expected_hold_pid")
fi
if [ "${#live_driver_pids[@]}" -eq 0 ] &&
	[ "$driver_state" != MISSING ] && [ "$driver_state" != INVALID ] &&
	[ "$driver_start" = "$expected_driver_start" ] &&
	recheck_original_live driver "$expected_driver_pid" "$expected_driver_start"; then
	live_driver_pids+=("$expected_driver_pid")
fi

join_pids() {
	joined=-
	shift
	if [ "$#" -gt 0 ]; then
		local old_ifs=$IFS
		IFS=,
		joined="$*"
		IFS=$old_ifs
	fi
}
join_pids live "${live_hold_pids[@]}"
live_holds=$joined
join_pids live "${live_driver_pids[@]}"
live_drivers=$joined
join_pids live "${zombie_pids[@]}"
zombies=$joined

# Referencing the expected starttimes makes their presence part of the shell
# contract; Go compares them with the independently observed values below.
: "$expected_hold_start" "$expected_driver_start"
printf 'SAMPLE\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n' \
	"$expected_hold_pid" "$hold_start" "$hold_state" \
	"$expected_driver_pid" "$driver_start" "$driver_state" \
	"$live_holds" "$live_drivers" "$zombies"
`

// ProbeCanaryHold proves that the exact workflow canary hold is running in the
// exact deterministic spawn pod. RBAC/exec errors and ambiguous or malformed
// observations fail closed; no matching process is a normal not-ready sample.
func (h *Harness) ProbeCanaryHold(ctx context.Context, spawnID string) (CanaryHoldObservation, bool, error) {
	if strings.TrimSpace(spawnID) == "" {
		return CanaryHoldObservation{}, false, fmt.Errorf("probe canary hold: spawn id is required")
	}

	podName := "spawn-" + spawnID
	duration := strconv.Itoa(workflow.CanaryHoldSeconds)
	raw, err := h.kubectl(ctx,
		"-n", h.cfg.SpawnNS,
		"exec", podName,
		"-c", "devbox",
		"--", "bash", "-c", canaryHoldProbeScript,
		"mills-canary-hold-probe", duration,
	)
	if err != nil {
		return CanaryHoldObservation{}, false, fmt.Errorf("probe canary hold in pod %s: %w", podName, err)
	}

	pid, startTime, driverPID, driverStartTime, ready, err := parseCanaryHoldProbeOutput(raw, workflow.CanaryHoldSeconds)
	if err != nil {
		return CanaryHoldObservation{}, false, fmt.Errorf("probe canary hold in pod %s: %w", podName, err)
	}
	if !ready {
		return CanaryHoldObservation{}, false, nil
	}

	return CanaryHoldObservation{
		PodName:              podName,
		PID:                  pid,
		StartTimeTicks:       startTime,
		DriverPID:            driverPID,
		DriverStartTimeTicks: driverStartTime,
		Seconds:              workflow.CanaryHoldSeconds,
		ObservedAt:           time.Now().UTC(),
	}, true, nil
}

func parseCanaryHoldProbeOutput(
	raw string,
	expectedSeconds int,
) (pid int, startTime uint64, driverPID int, driverStartTime uint64, ready bool, err error) {
	output := strings.TrimSpace(raw)
	if output == "NOT_READY" {
		return 0, 0, 0, 0, false, nil
	}

	fields := strings.Split(output, "\t")
	if len(fields) == 2 && fields[0] == "AMBIGUOUS" {
		count, parseErr := strconv.Atoi(fields[1])
		if parseErr != nil || count < 2 || fields[1] != strconv.Itoa(count) {
			return 0, 0, 0, 0, false, fmt.Errorf("malformed ambiguous-process result %q", output)
		}
		return 0, 0, 0, 0, false, fmt.Errorf("found %d exact canary hold processes", count)
	}
	if len(fields) == 3 && fields[0] == "DRIVER_MISMATCH" {
		count, countErr := parseCanonicalNonNegativeInt(fields[1])
		parentPID, parentErr := parseCanonicalProcessPID(fields[2])
		if countErr != nil || parentErr != nil {
			return 0, 0, 0, 0, false, fmt.Errorf("malformed completion-driver result %q", output)
		}
		return 0, 0, 0, 0, false, fmt.Errorf(
			"exact canary hold parent pid %d is not the unique live completion wrapper (found %d wrappers)",
			parentPID, count,
		)
	}
	if len(fields) != 6 || fields[0] != "READY" {
		return 0, 0, 0, 0, false, fmt.Errorf("malformed process result %q", output)
	}

	pid, err = parseCanonicalProcessPID(fields[1])
	if err != nil {
		return 0, 0, 0, 0, false, fmt.Errorf("malformed canary hold pid %q", fields[1])
	}
	startTime, err = parseCanonicalStartTime(fields[2], false)
	if err != nil {
		return 0, 0, 0, 0, false, fmt.Errorf("malformed canary hold starttime %q", fields[2])
	}
	seconds, err := strconv.Atoi(fields[3])
	if err != nil || seconds != expectedSeconds || fields[3] != strconv.Itoa(seconds) {
		return 0, 0, 0, 0, false, fmt.Errorf("malformed canary hold duration %q (want %d)", fields[3], expectedSeconds)
	}
	driverPID, err = parseCanonicalProcessPID(fields[4])
	if err != nil || driverPID == pid {
		return 0, 0, 0, 0, false, fmt.Errorf("malformed canary completion driver pid %q", fields[4])
	}
	driverStartTime, err = parseCanonicalStartTime(fields[5], false)
	if err != nil {
		return 0, 0, 0, 0, false, fmt.Errorf("malformed canary completion driver starttime %q", fields[5])
	}
	return pid, startTime, driverPID, driverStartTime, true, nil
}

// ProbeCanaryProcesses snapshots the original process states and every live
// exact canary hold/completion wrapper in the deterministic spawn pod.
func (h *Harness) ProbeCanaryProcesses(
	ctx context.Context,
	spawnID string,
	expectedHoldPID int,
	expectedHoldStart uint64,
	expectedDriverPID int,
	expectedDriverStart uint64,
) (CanaryProcessSample, error) {
	if strings.TrimSpace(spawnID) == "" {
		return CanaryProcessSample{}, fmt.Errorf("probe canary processes: spawn id is required")
	}
	if expectedHoldPID <= 1 {
		return CanaryProcessSample{}, fmt.Errorf("probe canary processes: hold pid must be greater than 1")
	}
	if expectedHoldStart == 0 {
		return CanaryProcessSample{}, fmt.Errorf("probe canary processes: hold starttime must be positive")
	}
	if expectedDriverPID <= 1 {
		return CanaryProcessSample{}, fmt.Errorf("probe canary processes: driver pid must be greater than 1")
	}
	if expectedDriverStart == 0 {
		return CanaryProcessSample{}, fmt.Errorf("probe canary processes: driver starttime must be positive")
	}
	if expectedHoldPID == expectedDriverPID {
		return CanaryProcessSample{}, fmt.Errorf("probe canary processes: hold and driver pids must differ")
	}

	podName := "spawn-" + spawnID
	observedAt := time.Now().UTC()
	raw, err := h.kubectl(ctx,
		"-n", h.cfg.SpawnNS,
		"exec", podName,
		"-c", "devbox",
		"--", "bash", "-c", canaryProcessProbeScript,
		"mills-canary-process-probe",
		strconv.Itoa(workflow.CanaryHoldSeconds),
		strconv.Itoa(expectedHoldPID),
		strconv.FormatUint(expectedHoldStart, 10),
		strconv.Itoa(expectedDriverPID),
		strconv.FormatUint(expectedDriverStart, 10),
	)
	if err != nil {
		return CanaryProcessSample{}, fmt.Errorf("probe canary processes in pod %s: %w", podName, err)
	}

	sample, err := parseCanaryProcessProbeOutput(raw, expectedHoldPID, expectedDriverPID)
	if err != nil {
		return CanaryProcessSample{}, fmt.Errorf("probe canary processes in pod %s: %w", podName, err)
	}
	sample.PodName = podName
	sample.ObservedAt = observedAt
	return sample, nil
}

func parseCanaryProcessProbeOutput(
	raw string,
	expectedHoldPID int,
	expectedDriverPID int,
) (CanaryProcessSample, error) {
	output := strings.TrimSpace(raw)
	fields := strings.Split(output, "\t")
	if len(fields) != 10 || fields[0] != "SAMPLE" {
		return CanaryProcessSample{}, fmt.Errorf("malformed process sample %q", output)
	}

	holdPID, err := parseCanonicalProcessPID(fields[1])
	if err != nil || holdPID != expectedHoldPID {
		return CanaryProcessSample{}, fmt.Errorf(
			"malformed process sample hold pid %q (want %d)", fields[1], expectedHoldPID)
	}
	holdStartTime, err := parseCanonicalStartTime(fields[2], true)
	if err != nil {
		return CanaryProcessSample{}, fmt.Errorf("malformed process sample hold starttime %q", fields[2])
	}
	holdState, err := parseCanaryProcessState(fields[3])
	if err != nil || (holdState == "MISSING") != (holdStartTime == 0) {
		return CanaryProcessSample{}, fmt.Errorf("malformed process sample hold identity %q/%q", fields[2], fields[3])
	}
	driverPID, err := parseCanonicalProcessPID(fields[4])
	if err != nil || driverPID != expectedDriverPID {
		return CanaryProcessSample{}, fmt.Errorf(
			"malformed process sample driver pid %q (want %d)", fields[4], expectedDriverPID)
	}
	driverStartTime, err := parseCanonicalStartTime(fields[5], true)
	if err != nil {
		return CanaryProcessSample{}, fmt.Errorf("malformed process sample driver starttime %q", fields[5])
	}
	driverState, err := parseCanaryProcessState(fields[6])
	if err != nil || (driverState == "MISSING") != (driverStartTime == 0) {
		return CanaryProcessSample{}, fmt.Errorf("malformed process sample driver identity %q/%q", fields[5], fields[6])
	}
	liveHoldPIDs, err := parseCanonicalPIDList(fields[7])
	if err != nil {
		return CanaryProcessSample{}, fmt.Errorf("malformed live hold pid inventory %q: %w", fields[7], err)
	}
	liveDriverPIDs, err := parseCanonicalPIDList(fields[8])
	if err != nil {
		return CanaryProcessSample{}, fmt.Errorf("malformed live driver pid inventory %q: %w", fields[8], err)
	}
	zombiePIDs, err := parseCanonicalPIDList(fields[9])
	if err != nil {
		return CanaryProcessSample{}, fmt.Errorf("malformed zombie pid inventory %q: %w", fields[9], err)
	}

	return CanaryProcessSample{
		HoldPID:              holdPID,
		HoldStartTimeTicks:   holdStartTime,
		HoldState:            holdState,
		DriverPID:            driverPID,
		DriverStartTimeTicks: driverStartTime,
		DriverState:          driverState,
		LiveHoldPIDs:         liveHoldPIDs,
		LiveDriverPIDs:       liveDriverPIDs,
		ZombiePIDs:           zombiePIDs,
	}, nil
}

func parseCanaryProcessState(raw string) (string, error) {
	if raw == "MISSING" {
		return raw, nil
	}
	switch raw {
	case "R", "S", "D", "Z", "T", "t", "X", "x", "K", "W", "P", "I":
		return raw, nil
	default:
		return "", fmt.Errorf("unknown process state")
	}
}

func isCanaryExecutionState(state string) bool {
	switch state {
	case "R", "S", "D":
		return true
	default:
		return false
	}
}

func parseCanonicalPIDList(raw string) ([]int, error) {
	if raw == "-" {
		return []int{}, nil
	}
	if raw == "" {
		return nil, fmt.Errorf("empty inventory")
	}

	parts := strings.Split(raw, ",")
	pids := make([]int, 0, len(parts))
	seen := make(map[int]struct{}, len(parts))
	for _, part := range parts {
		pid, err := parseCanonicalProcessPID(part)
		if err != nil {
			return nil, err
		}
		if _, ok := seen[pid]; ok {
			return nil, fmt.Errorf("duplicate pid %d", pid)
		}
		seen[pid] = struct{}{}
		pids = append(pids, pid)
	}
	sort.Ints(pids)
	return pids, nil
}

func parseCanonicalProcessPID(raw string) (int, error) {
	pid, err := strconv.Atoi(raw)
	if err != nil || pid <= 1 || raw != strconv.Itoa(pid) {
		return 0, fmt.Errorf("invalid process pid %q", raw)
	}
	return pid, nil
}

func parseCanonicalStartTime(raw string, allowZero bool) (uint64, error) {
	value, err := strconv.ParseUint(raw, 10, 64)
	if err != nil || raw != strconv.FormatUint(value, 10) || (!allowZero && value == 0) {
		return 0, fmt.Errorf("invalid process starttime %q", raw)
	}
	return value, nil
}

func parseCanonicalNonNegativeInt(raw string) (int, error) {
	value, err := strconv.Atoi(raw)
	if err != nil || value < 0 || raw != strconv.Itoa(value) {
		return 0, fmt.Errorf("invalid non-negative integer %q", raw)
	}
	return value, nil
}
