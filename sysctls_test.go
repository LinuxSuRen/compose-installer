package main

import (
	"errors"
	"strconv"
	"strings"
	"testing"
)

func TestParsePortRange(t *testing.T) {
	cases := []struct {
		in    string
		lo    int
		hi    int
		fails bool
	}{
		{in: "32768\t60999\n", lo: 32768, hi: 60999},
		{in: "1024 65000", lo: 1024, hi: 65000},
		{in: "1024", fails: true},
		{in: "a b", fails: true},
		{in: "60000 32768", fails: true}, // hi <= lo
		{in: "1 70000", fails: true},     // above 65535
	}
	for _, c := range cases {
		lo, hi, err := parsePortRange(c.in)
		if c.fails {
			if err == nil {
				t.Errorf("parsePortRange(%q) should fail, got %d-%d", c.in, lo, hi)
			}
			continue
		}
		if err != nil || lo != c.lo || hi != c.hi {
			t.Errorf("parsePortRange(%q) = %d,%d,%v want %d,%d", c.in, lo, hi, err, c.lo, c.hi)
		}
	}
}

func TestKernelQuotaDiagnosis(t *testing.T) {
	// Healthy: both items meet the bar; broken advisory items do not affect
	// the verdict.
	ok := &kernelQuotaDiagnosis{FileMax: minFileMax, Somaxconn: 4096, Overcommit: 0, PortLo: 32768, PortHi: 60999, PortsRead: true}
	if !ok.Healthy() {
		t.Error("both items meeting the bar should be healthy")
	}
	if cmds := ok.FixCommands(); cmds != nil {
		t.Errorf("healthy state must not produce repair commands: %v", cmds)
	}
	if adv := ok.advisories(); len(adv) != 0 {
		t.Errorf("overcommit=0 (not read) must not advise: %v", adv)
	}

	// Insufficient: both low -> two sysctl -w + one tee
	low := &kernelQuotaDiagnosis{FileMax: 98304, Somaxconn: 128, Overcommit: 1, PortsRead: true, PortLo: 32768, PortHi: 60999}
	if low.Healthy() {
		t.Error("98304/128 should be unhealthy")
	}
	cmds := low.FixCommands()
	if len(cmds) != 3 {
		t.Fatalf("want 3 repair commands, got %d: %v", len(cmds), cmds)
	}
	joined := strings.Join(cmds, "\n")
	for _, want := range []string{
		"sudo sysctl -w fs.file-max=" + strconv.Itoa(minFileMax),
		"sudo sysctl -w net.core.somaxconn=" + strconv.Itoa(minSomaxconn),
		"tee " + limitsConfPath,
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("repair command missing: %s (got: %v)", want, cmds)
		}
	}
	if s := low.Summary(); !strings.Contains(s, "[error]") {
		t.Errorf("insufficient verdict must contain the error marker: %s", s)
	}

	// Advisory items: overcommit != 1 / narrow port range -> advised but do
	// not affect Healthy.
	adv := &kernelQuotaDiagnosis{FileMax: minFileMax, Somaxconn: minSomaxconn, Overcommit: 2, PortsRead: true, PortLo: 40000, PortHi: 40960}
	if !adv.Healthy() {
		t.Error("advisory items must not affect the health verdict")
	}
	list := adv.advisories()
	if len(list) != 2 {
		t.Fatalf("want 2 advisories, got %d: %v", len(list), list)
	}
	if !strings.Contains(list[0], "vm.overcommit_memory=2") || !strings.Contains(list[1], "ip_local_port_range=40000-40960") {
		t.Errorf("unexpected advisory content: %v", list)
	}

	// Read failure: unhealthy and no repair commands.
	errd := &kernelQuotaDiagnosis{Err: errors.New("sysctl: not found")}
	if errd.Healthy() {
		t.Error("read failure should be unhealthy")
	}
	if cmds := errd.FixCommands(); cmds != nil {
		t.Errorf("read failure must not produce repair commands: %v", cmds)
	}
}

func TestKernelQuotaFixNeverLowers(t *testing.T) {
	// Host-customized higher values: healthy, zero repair commands; the
	// persisted content does not degrade.
	high := &kernelQuotaDiagnosis{FileMax: 2097152, Somaxconn: 8192, PortsRead: true, PortLo: 32768, PortHi: 60999}
	if !high.Healthy() {
		t.Error("above the minimums should be healthy")
	}
	if cmds := high.FixCommands(); cmds != nil {
		t.Errorf("above the minimums must not produce repair commands: %v", cmds)
	}
	content := limitsConfContent(2097152, 8192)
	for _, want := range []string{"fs.file-max=2097152", "net.core.somaxconn=8192"} {
		if !strings.Contains(content, want) {
			t.Errorf("persisted content missing %s (content: %s)", want, content)
		}
	}
	// Only one item insufficient: the other keeps its current high value and
	// does not fall back.
	partial := &kernelQuotaDiagnosis{FileMax: 98304, Somaxconn: 8192, PortsRead: true, PortLo: 32768, PortHi: 60999}
	cmds := partial.FixCommands()
	if len(cmds) != 2 { // one sysctl -w + one tee
		t.Fatalf("should repair only the insufficient item (2 commands), got %d: %v", len(cmds), cmds)
	}
	if !strings.Contains(cmds[1], "net.core.somaxconn=8192") {
		t.Errorf("persisted content must not fall the already-sufficient somaxconn back to the minimum: %v", cmds)
	}
	if strings.Contains(cmds[0], "somaxconn") {
		t.Errorf("the already-sufficient somaxconn must not run sysctl -w again: %v", cmds)
	}
}
