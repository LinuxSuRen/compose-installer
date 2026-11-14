// inotify quota diagnosis and self-healing: a class of host-level root
// causes behind container startup failures.
//
// fs.inotify.max_user_instances / fs.inotify.max_user_watches are host
// kernel globals: counted by real uid, not namespacable, immutable from
// inside containers, and shared by every container and host process running
// as the same uid (containers default to root). Kernel defaults are tiny
// (instances=128, watches commonly 8192~65536); once other processes (IDEs,
// other docker projects, cadvisor-like monitors) eat up the quota, container
// startup fails at inotify_init / add_watch with the typical ENOSPC ("No
// space left on device") or EMFILE ("Too many open files"), and the process
// crash-loops from the first start.
//
// Two entry points:
//  1. install precheck (install.go step 5.7): raise in place and persist
//     when below threshold
//  2. doctor subcommand (main.go): standalone diagnosis printing current
//     values and repair commands
//
// Principles: only raise, never lower (when the host already customized a
// higher value, persist that value); idempotent (repeated runs have no side
// effects); effective immediately (sysctl -w, no docker/container restart
// needed).
package main

import (
	"context"
	"fmt"
	"strconv"
	"strings"
)

// inotify quota recommended minimums and the persistence file path (sysctl.d
// applies files in lexicographic order, so the 99- prefix takes effect after
// the distro default files). Threshold rationale: measured headroom for a
// single-machine platform stack plus monitoring, a dozen containers
// (instances 512 = 4x the default; watches 1048576 is the common industry
// value — watch memory is allocated on demand, on the order of a hundred
// bytes each, so the real cost stays far below the theoretical cap).
const (
	inotifyMinInstances = 512
	inotifyMinWatches   = 1048576
	inotifyConfPath     = "/etc/sysctl.d/99-compose-installer-inotify.conf"
)

// inotifyDiagnosis is the result of one inotify quota diagnosis. Current == 0
// means that item's read failed (treated as insufficient; the fix action
// will try to set it and surface the real error).
type inotifyDiagnosis struct {
	Instances int
	Watches   int
	Err       error // why the sysctl read failed (non-Linux / no sysctl etc.); nil = normal
}

// readSysctlInt reads one integer sysctl (missing/non-numeric returns an
// error).
func readSysctlInt(ctx context.Context, runner Runner, key string) (int, error) {
	out, err := runner.Capture(ctx, "", fmt.Sprintf(`sysctl -n %s 2>/dev/null`, key), nil)
	if err != nil {
		return 0, err
	}
	v, perr := strconv.Atoi(strings.TrimSpace(out))
	if perr != nil {
		return 0, fmt.Errorf("%s output is not numeric: %q", key, strings.TrimSpace(out))
	}
	return v, nil
}

// diagnoseInotify reads the current inotify quotas (read-only, repeatable).
func diagnoseInotify(ctx context.Context, runner Runner) (*inotifyDiagnosis, error) {
	d := &inotifyDiagnosis{}
	inst, ierr := readSysctlInt(ctx, runner, "fs.inotify.max_user_instances")
	watch, werr := readSysctlInt(ctx, runner, "fs.inotify.max_user_watches")
	d.Instances, d.Watches = inst, watch
	switch {
	case ierr != nil && werr != nil:
		d.Err = fmt.Errorf("cannot read fs.inotify.* (%v / %v; this check only supports Linux hosts)", ierr, werr)
	case ierr != nil:
		d.Err = ierr
	case werr != nil:
		d.Err = werr
	}
	return d, d.Err
}

// Healthy reports whether both quotas are at least at the recommended
// minimums.
func (d *inotifyDiagnosis) Healthy() bool {
	return d.Err == nil && d.Instances >= inotifyMinInstances && d.Watches >= inotifyMinWatches
}

// inotifyDesired only raises, never lowers: keep the current value when it
// is above the minimum.
func inotifyDesired(current, min int) int {
	if current > min {
		return current
	}
	return min
}

// inotifyConfContent renders the persistence file content (both items are
// always written as a pair, so the other one never falls back to the distro
// default).
func inotifyConfContent(instances, watches int) string {
	return fmt.Sprintf(
		"# managed by compose-installer: inotify quota for containers (only raised, never lowered)\n"+
			"fs.inotify.max_user_instances=%d\n"+
			"fs.inotify.max_user_watches=%d\n", instances, watches)
}

// Summary is the human-readable verdict (shared by doctor output / install
// logs).
func (d *inotifyDiagnosis) Summary() string {
	var b strings.Builder
	fmt.Fprintf(&b, "fs.inotify.max_user_instances=%d (recommended >=%d)\n", d.Instances, inotifyMinInstances)
	fmt.Fprintf(&b, "fs.inotify.max_user_watches=%d (recommended >=%d)", d.Watches, inotifyMinWatches)
	if d.Err != nil {
		return b.String()
	}
	if !d.Healthy() {
		fmt.Fprintln(&b)
		fmt.Fprintln(&b, "[error] inotify quota below the recommended values: in-container file watchers (frontend hot reload, algorithm task watchers, cadvisor-like monitors)")
		fmt.Fprintln(&b, "       share one quota pool with the host; on exhaustion the container fails at startup (ENOSPC / Too many open files -> crash loop).")
		fmt.Fprintln(&b, "       The parameters are host-kernel globals (not namespacable): they can only be raised on the host; containers cannot self-heal.")
	}
	return b.String()
}

// FixCommands returns directly executable repair commands for the current
// diagnosis (for doctor output). Returns nil when the quotas are sufficient;
// sysctl -w is given only for insufficient items; the persistence file is
// written with max(current, minimum) (only raised, never lowered).
func (d *inotifyDiagnosis) FixCommands() []string {
	if d.Err != nil || d.Healthy() {
		return nil
	}
	var cmds []string
	inst, watch := inotifyDesired(d.Instances, inotifyMinInstances), inotifyDesired(d.Watches, inotifyMinWatches)
	if d.Instances < inotifyMinInstances {
		cmds = append(cmds, fmt.Sprintf("sudo sysctl -w fs.inotify.max_user_instances=%d", inst))
	}
	if d.Watches < inotifyMinWatches {
		cmds = append(cmds, fmt.Sprintf("sudo sysctl -w fs.inotify.max_user_watches=%d", watch))
	}
	cmds = append(cmds, fmt.Sprintf("printf '%s' | sudo tee %s",
		strings.ReplaceAll(inotifyConfContent(inst, watch), "\n", "\\n"), inotifyConfPath))
	return cmds
}

// ensureInotifyLimits is install-flow step 5.7: raise insufficient quotas in
// place and persist them. Idempotent: zero actions when the quotas suffice;
// already persisted with identical content -> no rewrite (writeRootFile
// skips by sha256). The caller only warns and does not block the install on
// error (doctor supports self-service repair).
func ensureInotifyLimits(ctx context.Context, runner Runner, j *Job, sudo string) error {
	d, err := diagnoseInotify(ctx, runner)
	if err != nil {
		return fmt.Errorf("failed to read inotify quotas: %w", err)
	}
	if d.Healthy() {
		j.Logf("inotify quotas sufficient (instances=%d, watches=%d), no adjustment needed", d.Instances, d.Watches)
		return nil
	}
	inst, watch := inotifyDesired(d.Instances, inotifyMinInstances), inotifyDesired(d.Watches, inotifyMinWatches)
	if d.Instances < inotifyMinInstances {
		if _, e := runOut(ctx, runner, fmt.Sprintf(`%s sysctl -w fs.inotify.max_user_instances=%d`, sudo, inst)); e != nil {
			return fmt.Errorf("failed to raise fs.inotify.max_user_instances: %w", e)
		}
		j.Logf("fs.inotify.max_user_instances: %d -> %d", d.Instances, inst)
	}
	if d.Watches < inotifyMinWatches {
		if _, e := runOut(ctx, runner, fmt.Sprintf(`%s sysctl -w fs.inotify.max_user_watches=%d`, sudo, watch)); e != nil {
			return fmt.Errorf("failed to raise fs.inotify.max_user_watches: %w", e)
		}
		j.Logf("fs.inotify.max_user_watches: %d -> %d", d.Watches, watch)
	}
	// Persist (survives reboot): both items written as a pair and only
	// raised, never lowered, so the other item never falls back and a
	// host-customized higher value is never reduced.
	if e := writeRootFile(ctx, runner, sudo, inotifyConfPath, inotifyConfContent(inst, watch)); e != nil {
		return fmt.Errorf("failed to write %s: %w", inotifyConfPath, e)
	}
	// Re-read to verify: make sure the runtime values really took effect.
	d2, err := diagnoseInotify(ctx, runner)
	if err != nil || !d2.Healthy() {
		return fmt.Errorf("inotify quota verification after adjustment failed (instances=%d, watches=%d, err=%v)", d2.Instances, d2.Watches, err)
	}
	j.Logf("inotify quotas raised and persisted to %s (survives reboot)", inotifyConfPath)
	return nil
}
