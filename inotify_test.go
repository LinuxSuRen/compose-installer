package main

import (
	"errors"
	"strconv"
	"strings"
	"testing"
)

func TestInotifyDiagnosisHealthy(t *testing.T) {
	ok := &inotifyDiagnosis{Instances: inotifyMinInstances, Watches: inotifyMinWatches}
	if !ok.Healthy() {
		t.Error("quotas exactly at the minimums should be healthy")
	}
	if s := ok.Summary(); strings.Contains(s, "[error]") {
		t.Errorf("healthy verdict must not contain the error marker: %s", s)
	}
	if cmds := ok.FixCommands(); cmds != nil {
		t.Errorf("healthy state must not produce repair commands: %v", cmds)
	}

	low := &inotifyDiagnosis{Instances: 128, Watches: 8192}
	if low.Healthy() {
		t.Error("128/8192 should be unhealthy")
	}
	if s := low.Summary(); !strings.Contains(s, "[error]") || !strings.Contains(s, "ENOSPC") {
		t.Errorf("unhealthy verdict missing the root-cause signature: %s", s)
	}
	cmds := low.FixCommands()
	if len(cmds) != 3 { // two sysctl -w + one tee to persist
		t.Fatalf("want 3 repair commands, got %d: %v", len(cmds), cmds)
	}
	for _, want := range []string{
		"sudo sysctl -w fs.inotify.max_user_instances=" + strconv.Itoa(inotifyMinInstances),
		"sudo sysctl -w fs.inotify.max_user_watches=" + strconv.Itoa(inotifyMinWatches),
		"tee " + inotifyConfPath,
	} {
		if !strings.Contains(strings.Join(cmds, "\n"), want) {
			t.Errorf("repair command missing: %s (got: %v)", want, cmds)
		}
	}

	// Read failure (non-Linux / no sysctl): unhealthy and no repair commands
	// (nothing to judge on).
	errd := &inotifyDiagnosis{Err: errors.New("sysctl: not found")}
	if errd.Healthy() {
		t.Error("read failure should be unhealthy")
	}
	if cmds := errd.FixCommands(); cmds != nil {
		t.Errorf("read failure must not produce repair commands: %v", cmds)
	}
}

func TestInotifyFixNeverLowers(t *testing.T) {
	if got := inotifyDesired(2048, inotifyMinInstances); got != 2048 {
		t.Errorf("above the minimum should keep the current value, got %d", got)
	}
	if got := inotifyDesired(100, inotifyMinInstances); got != inotifyMinInstances {
		t.Errorf("below the minimum should take the minimum, got %d", got)
	}
	// Host-customized higher values: healthy, zero repair commands.
	high := &inotifyDiagnosis{Instances: 2048, Watches: 8388608}
	if !high.Healthy() {
		t.Error("above the minimums should be healthy")
	}
	if cmds := high.FixCommands(); cmds != nil {
		t.Errorf("above the minimums must not produce repair commands: %v", cmds)
	}
	// Persisted content written as a pair and never lowered.
	content := inotifyConfContent(2048, 8388608)
	for _, want := range []string{
		"fs.inotify.max_user_instances=2048",
		"fs.inotify.max_user_watches=8388608",
	} {
		if !strings.Contains(content, want) {
			t.Errorf("persisted content missing %s (content: %s)", want, content)
		}
	}
	// Only one item insufficient: the other keeps its current high value and
	// does not fall back to the minimum.
	partial := &inotifyDiagnosis{Instances: 128, Watches: 8388608}
	cmds := partial.FixCommands()
	if len(cmds) != 2 { // one sysctl -w + one tee
		t.Fatalf("should repair only the insufficient item (2 commands), got %d: %v", len(cmds), cmds)
	}
	if !strings.Contains(cmds[1], "fs.inotify.max_user_watches=8388608") {
		t.Errorf("persisted content must not fall the already-sufficient watches back to the minimum: %v", cmds)
	}
	if strings.Contains(cmds[1], "sudo sysctl -w fs.inotify.max_user_watches") {
		t.Errorf("the already-sufficient watches must not run sysctl -w again: %v", cmds)
	}
}
