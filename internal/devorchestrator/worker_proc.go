package devorchestrator

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Host-process observation for Worker residue detection (T0011). Everything
// here reads /proc directly — no external binaries, no root: a Worker's
// leftovers run under the same uid as rddev, so their /proc state is
// readable. The two signals are the Worker's session id (everything spawn
// starts inherits the reaper's session, because the reaper is launched with
// setsid) and the listening-socket baseline (a daemonized service escapes the
// session but keeps its listener).

// procStatFields returns the fields of /proc/<pid>/stat after the comm field
// (comm may contain spaces and parens, so parsing restarts after the last
// ')' — the same fix pidAlive uses).
func procStatFields(pid int) ([]string, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return nil, err
	}
	rest := data
	if i := bytes.LastIndexByte(rest, ')'); i >= 0 {
		rest = rest[i+1:]
	}
	return strings.Fields(string(rest)), nil
}

// markerResidue returns every live process whose environment still carries
// POST_WORKER_RUN_ID=<runID> and that started after startTicks. This is the
// attribution signal that catches what the session scan cannot (T0011
// Defect 2): real claude's Bash tool runs each command in its own session,
// so a nohup'd survivor escapes the reaper's session entirely. The run
// marker spawn put into the Worker environment travels into every descendant
// unless the descendant scrubs its environment — a process that scrubbed it
// is not attributable, exactly like a daemon that escaped the session.
// pids already reported (session findings, the Worker itself) are excluded.
func markerResidue(runID string, startTicks uint64, exclude map[int]bool) ([]ProcessFinding, error) {
	if runID == "" {
		return []ProcessFinding{}, nil
	}
	marker := "POST_WORKER_RUN_ID=" + runID
	pids, err := procPids()
	if err != nil {
		return nil, err
	}
	out := []ProcessFinding{}
	for _, pid := range pids {
		if exclude[pid] {
			continue
		}
		fields, err := procStatFields(pid)
		if err != nil {
			continue // exited between the listing and the read
		}
		if len(fields) < 20 || fields[0] == "Z" {
			continue // zombies are dead, not residue (same rule as the session scan)
		}
		ticks, err := strconv.ParseUint(fields[19], 10, 64)
		if err != nil || ticks < startTicks {
			// strictly-before cutoff: a descendant may start in the same
			// 10ms clock tick as the Worker, and a same-tick non-descendant
			// cannot hold the run-unique marker, so same-tick is attributable
			continue
		}
		environ, err := os.ReadFile(fmt.Sprintf("/proc/%d/environ", pid))
		if err != nil {
			continue // another uid's process or already gone: not attributable
		}
		hasMarker := false
		for _, kv := range bytes.Split(environ, []byte{0}) {
			if string(kv) == marker {
				hasMarker = true
				break
			}
		}
		if !hasMarker {
			continue
		}
		out = append(out, ProcessFinding{
			PID:     pid,
			Cmdline: procCmdline(pid),
			Session: sessionOf(fields),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PID < out[j].PID })
	return out, nil
}

// sessionOf extracts the session id from parsed /proc/<pid>/stat fields
// (field 6 of stat, index 3 after the comm field). 0 when unparsable.
func sessionOf(fields []string) int {
	if len(fields) < 4 {
		return 0
	}
	sess, err := strconv.Atoi(fields[3])
	if err != nil {
		return 0
	}
	return sess
}

// procSession returns the session id of pid (/proc/<pid>/stat field 6, the
// 4th field after the comm field: state, ppid, pgrp, session).
func procSession(pid int) (int, error) {
	f, err := procStatFields(pid)
	if err != nil {
		return 0, err
	}
	if len(f) < 4 {
		return 0, fmt.Errorf("/proc/%d/stat has only %d fields after the command", pid, len(f))
	}
	return strconv.Atoi(f[3])
}

// procStartTicks returns the process start time in clock ticks (/proc/<pid>/
// stat field 22, index 19 after the comm field).
func procStartTicks(pid int) (uint64, error) {
	f, err := procStatFields(pid)
	if err != nil {
		return 0, err
	}
	if len(f) < 20 {
		return 0, fmt.Errorf("/proc/%d/stat has only %d fields after the command", pid, len(f))
	}
	return strconv.ParseUint(f[19], 10, 64)
}

// procCmdline reads /proc/<pid>/cmdline and joins its NUL-separated args with
// spaces (best effort; an empty read means the process is already gone).
func procCmdline(pid int) string {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		return ""
	}
	data = bytes.TrimRight(data, "\x00")
	if len(data) == 0 {
		return ""
	}
	return strings.ReplaceAll(string(data), "\x00", " ")
}

// procPids lists every numeric /proc entry (live pids).
func procPids() ([]int, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, fmt.Errorf("scanning /proc: %w", err)
	}
	pids := make([]int, 0, len(entries))
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		pids = append(pids, pid)
	}
	return pids, nil
}

// sessionResidue returns every live process whose session id equals
// sessionLeader, excluding the leader itself when it is the spawn reaper
// (run-worker.sh) still exiting — the reaper writes exit.status as its last
// act, so a collect in that instant must not read the spawn machinery as
// residue. The recorded Worker pid is excluded too: its liveness is the
// registry's business, not residue.
func sessionResidue(sessionLeader, workerPID int) ([]ProcessFinding, error) {
	pids, err := procPids()
	if err != nil {
		return nil, err
	}
	out := make([]ProcessFinding, 0)
	for _, pid := range pids {
		if pid == workerPID {
			continue
		}
		fields, err := procStatFields(pid)
		if err != nil {
			continue // exited between the listing and the read
		}
		if len(fields) < 4 {
			continue
		}
		if fields[0] == "Z" {
			// a zombie is already dead — its parent has simply not reaped
			// it. It is not a service the Worker left running and there is
			// nothing to "stop" (spawn never reaps the reaper wrapper, so
			// its zombie outlives the run on a long-lived rddev).
			continue
		}
		sess, err := strconv.Atoi(fields[3])
		if err != nil {
			continue
		}
		if sess != sessionLeader {
			continue
		}
		cmdline := procCmdline(pid)
		if pid == sessionLeader && strings.Contains(cmdline, "run-worker.sh") {
			continue // the reaper wrapping claude, exiting after exit.status
		}
		out = append(out, ProcessFinding{PID: pid, Cmdline: cmdline, Session: sess})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].PID < out[j].PID })
	return out, nil
}

// ListenerSnapshot returns the currently listening TCP sockets as
// "<addr-hex>:<port>" strings (the address exactly as /proc/net/tcp prints
// it, the port in decimal), one per LISTEN line of /proc/net/tcp{,6}, sorted.
// Spawn records it as the baseline; collect diffs against it.
func ListenerSnapshot() ([]string, error) {
	listeners, err := currentListeners()
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(listeners))
	for addr := range listeners {
		out = append(out, addr)
	}
	sort.Strings(out)
	return out, nil
}

type listenerInfo struct {
	Addr  string // "<addr-hex>:<port>", e.g. "0100007F:8080"
	Inode string
}

// currentListeners parses /proc/net/tcp{,6} for LISTEN sockets.
func currentListeners() (map[string]listenerInfo, error) {
	out := map[string]listenerInfo{}
	for _, f := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		data, err := os.ReadFile(f)
		if os.IsNotExist(err) {
			continue // no IPv6 support on some hosts; IPv4 is the baseline
		}
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", f, err)
		}
		info, err := parseTCPListeners(string(data))
		if err != nil {
			return nil, fmt.Errorf("parsing %s: %w", f, err)
		}
		for addr, li := range info {
			out[addr] = li
		}
	}
	return out, nil
}

// parseTCPListeners extracts LISTEN lines from /proc/net/tcp{,6} content:
// sl  local_address rem_address   st ... inode ...
//
//	0: 0100007F:1F90 00000000:0000 0A 00000000:00000000 00:00000000 ...
//
// The state field (index 3) must be 0A (LISTEN); the inode is the last
// field. Pure parsing, separated from the file reads so tests can feed it
// synthetic rows.
func parseTCPListeners(content string) (map[string]listenerInfo, error) {
	out := map[string]listenerInfo{}
	lines := strings.Split(content, "\n")
	for i, ln := range lines {
		if i == 0 || ln == "" {
			continue // the header line
		}
		f := strings.Fields(ln)
		if len(f) < 10 {
			return nil, fmt.Errorf("line %d has %d fields, want the /proc/net/tcp layout", i+1, len(f))
		}
		if f[3] != "0A" {
			continue
		}
		addrHex, portHex, ok := strings.Cut(f[1], ":")
		if !ok {
			return nil, fmt.Errorf("line %d: local address %q is not addr:port", i+1, f[1])
		}
		port, err := strconv.ParseUint(portHex, 16, 32)
		if err != nil {
			return nil, fmt.Errorf("line %d: port %q: %w", i+1, portHex, err)
		}
		out[fmt.Sprintf("%s:%d", addrHex, port)] = listenerInfo{
			Addr:  fmt.Sprintf("%s:%d", addrHex, port),
			Inode: f[9], // sl local rem st tx rx tr retrnsmt uid timeout inode
		}
	}
	return out, nil
}

// socketOwner finds the pid that holds the socket with the given inode by
// scanning /proc/<pid>/fd for "socket:[<inode>]" targets. ok is false when no
// owner is visible (socket owned by another uid or the process exited).
func socketOwner(inode string) (pid int, ok bool) {
	pids, err := procPids()
	if err != nil {
		return 0, false
	}
	target := "socket:[" + inode + "]"
	for _, pid := range pids {
		fdDir := fmt.Sprintf("/proc/%d/fd", pid)
		entries, err := os.ReadDir(fdDir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			link, err := os.Readlink(filepath.Join(fdDir, e.Name()))
			if err != nil {
				continue
			}
			if link == target {
				return pid, true
			}
		}
	}
	return 0, false
}
