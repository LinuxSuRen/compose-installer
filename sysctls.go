// Host kernel quota family diagnosis and self-healing: the same family of
// problems as inotify — kernel limits counted per host globally / per uid,
// immutable from inside containers, exhausted on low-quota machines during
// container startup or runtime.
//
// Two groups:
//   - auto-raisable (only raised, never lowered; idempotent — the same
//     pattern as inotify):
//     fs.file-max (host-global file handles; exhausted once many containers
//     run -> EMFILE "Too many open files"; tell it apart from the inotify
//     instances EMFILE via /proc/<pid>/limits),
//     net.core.somaxconn (listen queue backlog; bounds container published
//     ports and redis alike; old kernels default to 128, redis officially
//     recommends >= 511);
//   - advisory only, never changed automatically (the blast radius is too
//     large; left to human decision):
//     vm.overcommit_memory (when != 1, redis fork/bgrewrite can fail under
//     memory pressure),
//     net.ipv4.ip_local_port_range (too few outbound ports: with heavy
//     outbound traffic from backend/algorithm containers the ports exhaust
//     and connections fail randomly).
//
// Same entry points as inotify: install step 5.7 precheck + doctor
// subcommand.
package main

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

const (
	minFileMax        = 131072 // measured headroom for ~10 containers x node/python process handles
	minSomaxconn      = 1024   // redis recommends >=511; rounded to 1024
	minEphemeralPorts = 24576  // default 32768-60999 ~= 28k; warn when notably below
	limitsConfPath    = "/etc/sysctl.d/99-compose-installer-limits.conf"
	quotaProbeCount   = 4 // 3 integer sysctls + 1 port range
)

// kernelQuotaDiagnosis is the result of one quota-family diagnosis. A value
// of 0 means that item's read failed (auto-raisable items count as
// insufficient and the fix action surfaces the real error; advisory items
// are skipped).
type kernelQuotaDiagnosis struct {
	FileMax    int
	Somaxconn  int
	Overcommit int
	PortLo     int
	PortHi     int
	PortsRead  bool // whether ip_local_port_range was read successfully
	Err        error
}

// parsePortRange parses the output of `sysctl -n net.ipv4.ip_local_port_range`
// (shaped like "32768\t60999", tab/space separated).
func parsePortRange(out string) (int, int, error) {
	f := strings.Fields(strings.TrimSpace(out))
	if len(f) != 2 {
		return 0, 0, fmt.Errorf("output is not two fields: %q", strings.TrimSpace(out))
	}
	lo, e1 := strconv.Atoi(f[0])
	hi, e2 := strconv.Atoi(f[1])
	if e1 != nil || e2 != nil {
		return 0, 0, fmt.Errorf("output is not numeric: %q", strings.TrimSpace(out))
	}
	if lo <= 0 || hi <= lo || hi > 65535 {
		return 0, 0, fmt.Errorf("invalid port range: %q", strings.TrimSpace(out))
	}
	return lo, hi, nil
}

// diagnoseKernelQuotas reads the current quota family values (read-only,
// repeatable).
func diagnoseKernelQuotas(ctx context.Context, runner Runner) (*kernelQuotaDiagnosis, error) {
	d := &kernelQuotaDiagnosis{}
	var errs []string
	for k, dst := range map[string]*int{
		"fs.file-max":          &d.FileMax,
		"net.core.somaxconn":   &d.Somaxconn,
		"vm.overcommit_memory": &d.Overcommit,
	} {
		v, err := readSysctlInt(ctx, runner, k)
		if err != nil {
			errs = append(errs, fmt.Sprintf("%s: %v", k, err))
			continue
		}
		*dst = v
	}
	if out, err := runner.Capture(ctx, "", `sysctl -n net.ipv4.ip_local_port_range 2>/dev/null`, nil); err == nil {
		if lo, hi, perr := parsePortRange(out); perr == nil {
			d.PortLo, d.PortHi, d.PortsRead = lo, hi, true
		} else {
			errs = append(errs, fmt.Sprintf("net.ipv4.ip_local_port_range: %v", perr))
		}
	} else {
		errs = append(errs, fmt.Sprintf("net.ipv4.ip_local_port_range: %v", err))
	}
	if len(errs) == quotaProbeCount { // every probe failed: treat as undiagnosable (usually not Linux)
		d.Err = fmt.Errorf("cannot read kernel parameters (%s; this check only supports Linux hosts)", strings.Join(errs, "; "))
	}
	return d, d.Err
}

// Healthy reports whether both auto-raisable items are at least at the
// minimum (advisory items do not affect the verdict).
func (d *kernelQuotaDiagnosis) Healthy() bool {
	return d.Err == nil && d.FileMax >= minFileMax && d.Somaxconn >= minSomaxconn
}

// limitsConfContent renders the persistence file content (both items written
// as a pair and only raised, never lowered — same reasoning as inotify).
func limitsConfContent(fileMax, somaxconn int) string {
	return fmt.Sprintf(
		"# managed by compose-installer: kernel quota for containers (only raised, never lowered)\n"+
			"fs.file-max=%d\n"+
			"net.core.somaxconn=%d\n", fileMax, somaxconn)
}

// advisories returns the conclusion texts of the advisory-only items (nil
// when there is nothing to say).
func (d *kernelQuotaDiagnosis) advisories() []string {
	var out []string
	if d.Overcommit != 0 && d.Overcommit != 1 {
		out = append(out, fmt.Sprintf(
			"[advisory] vm.overcommit_memory=%d (!= 1): redis fork/bgrewrite may fail under memory pressure; if the host memory policy is adjustable, run sudo sysctl -w vm.overcommit_memory=1 and persist it into /etc/sysctl.d/",
			d.Overcommit))
	}
	if d.PortsRead && d.PortHi-d.PortLo+1 < minEphemeralPorts {
		out = append(out, fmt.Sprintf(
			"[advisory] net.ipv4.ip_local_port_range=%d-%d (only %d outbound ports): heavy outbound connections exhaust the ports and connections fail randomly; you can run sudo sysctl -w net.ipv4.ip_local_port_range=\"1024 65000\"",
			d.PortLo, d.PortHi, d.PortHi-d.PortLo+1))
	}
	return out
}

// Summary is the human-readable verdict (shared by doctor output / install
// logs).
func (d *kernelQuotaDiagnosis) Summary() string {
	var b strings.Builder
	fmt.Fprintf(&b, "fs.file-max=%d (recommended >=%d)\n", d.FileMax, minFileMax)
	fmt.Fprintf(&b, "net.core.somaxconn=%d (recommended >=%d)\n", d.Somaxconn, minSomaxconn)
	if d.PortsRead {
		fmt.Fprintf(&b, "net.ipv4.ip_local_port_range=%d-%d (recommended >=%d ports, advisory only)", d.PortLo, d.PortHi, minEphemeralPorts)
	} else {
		fmt.Fprintf(&b, "net.ipv4.ip_local_port_range=(not read, advisory only)")
	}
	if d.Err != nil {
		return b.String()
	}
	if !d.Healthy() {
		fmt.Fprintln(&b)
		fmt.Fprintln(&b, "[error] auto-raisable items below the recommended values: host-global file handles / listen backlog are shared by all containers;")
		fmt.Fprintln(&b, "       exhaustion shows up as EMFILE (Too many open files) or queued/refused connections, independently of the inotify quota.")
	}
	for _, a := range d.advisories() {
		fmt.Fprintln(&b, a)
	}
	return strings.TrimRight(b.String(), "\n")
}

// FixCommands returns directly executable repair commands for the current
// diagnosis (for doctor output); covers only the auto-raisable items — see
// Summary for the advisory ones.
func (d *kernelQuotaDiagnosis) FixCommands() []string {
	if d.Err != nil || d.Healthy() {
		return nil
	}
	var cmds []string
	fileMax := inotifyDesired(d.FileMax, minFileMax)
	somaxconn := inotifyDesired(d.Somaxconn, minSomaxconn)
	if d.FileMax < minFileMax {
		cmds = append(cmds, fmt.Sprintf("sudo sysctl -w fs.file-max=%d", fileMax))
	}
	if d.Somaxconn < minSomaxconn {
		cmds = append(cmds, fmt.Sprintf("sudo sysctl -w net.core.somaxconn=%d", somaxconn))
	}
	cmds = append(cmds, fmt.Sprintf("printf '%s' | sudo tee %s",
		strings.ReplaceAll(limitsConfContent(fileMax, somaxconn), "\n", "\\n"), limitsConfPath))
	return cmds
}

// ensureKernelQuotas is install-flow step 5.7: raise insufficient quotas in
// place and persist them (same pattern as ensureInotifyLimits); advisory
// items are only logged, never changed. The caller only warns on error.
func ensureKernelQuotas(ctx context.Context, runner Runner, j *Job, sudo string) error {
	d, err := diagnoseKernelQuotas(ctx, runner)
	if err != nil {
		return fmt.Errorf("failed to read kernel parameters: %w", err)
	}
	if d.Healthy() {
		j.Logf("kernel quotas sufficient (file-max=%d, somaxconn=%d), no adjustment needed", d.FileMax, d.Somaxconn)
	} else {
		fileMax := inotifyDesired(d.FileMax, minFileMax)
		somaxconn := inotifyDesired(d.Somaxconn, minSomaxconn)
		if d.FileMax < minFileMax {
			if _, e := runOut(ctx, runner, fmt.Sprintf(`%s sysctl -w fs.file-max=%d`, sudo, fileMax)); e != nil {
				return fmt.Errorf("failed to raise fs.file-max: %w", e)
			}
			j.Logf("fs.file-max: %d -> %d", d.FileMax, fileMax)
		}
		if d.Somaxconn < minSomaxconn {
			if _, e := runOut(ctx, runner, fmt.Sprintf(`%s sysctl -w net.core.somaxconn=%d`, sudo, somaxconn)); e != nil {
				return fmt.Errorf("failed to raise net.core.somaxconn: %w", e)
			}
			j.Logf("net.core.somaxconn: %d -> %d", d.Somaxconn, somaxconn)
		}
		if e := writeRootFile(ctx, runner, sudo, limitsConfPath, limitsConfContent(fileMax, somaxconn)); e != nil {
			return fmt.Errorf("failed to write %s: %w", limitsConfPath, e)
		}
		d2, err := diagnoseKernelQuotas(ctx, runner)
		if err != nil || !d2.Healthy() {
			return fmt.Errorf("kernel quota verification after adjustment failed (file-max=%d, somaxconn=%d, err=%v)", d2.FileMax, d2.Somaxconn, err)
		}
		j.Logf("kernel quotas raised and persisted to %s (survives reboot)", limitsConfPath)
	}
	for _, a := range d.advisories() {
		j.Logf("%s", a)
	}
	return nil
}
