package main

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestDaemonIptablesDisabled(t *testing.T) {
	cases := map[string]bool{
		`{"iptables": false}`: true,
		"{\n  \"data-root\": \"/data\",\n  \"iptables\": false\n}": true,
		`{"iptables": true}`:                          false,
		`{"data-root": "/x", "registry-mirrors": []}`: false,
		`{}`:       false,
		``:         false,
		`   `:      false,
		`not json`: false, // malformed JSON must not misfire; left to manual inspection
	}
	for raw, want := range cases {
		if got := daemonIptablesDisabled(raw); got != want {
			t.Errorf("daemonIptablesDisabled(%q) = %v, want %v", raw, got, want)
		}
	}
}

func TestStripIptablesFlagPreservesOtherConfig(t *testing.T) {
	raw := "{\n  \"data-root\": \"/data/docker\",\n  \"iptables\": false,\n  \"registry-mirrors\": [\"https://m.example.com\"]\n}"
	out, changed, err := stripIptablesFlag(raw)
	if err != nil || !changed {
		t.Fatalf("err=%v changed=%v", err, changed)
	}
	if daemonIptablesDisabled(out) {
		t.Error("iptables:false is still present")
	}
	for _, keep := range []string{"/data/docker", "m.example.com"} {
		if !strings.Contains(out, keep) {
			t.Errorf("existing setting lost: %s (output: %s)", keep, out)
		}
	}
	// Idempotent: a second pass is a no-op
	if _, changed2, err2 := stripIptablesFlag(out); err2 != nil || changed2 {
		t.Errorf("second pass is not idempotent: changed=%v err=%v", changed2, err2)
	}
	// iptables:true stays untouched
	if _, c3, _ := stripIptablesFlag(`{"iptables": true}`); c3 {
		t.Error("iptables:true must not be modified")
	}
}

func TestPickProbeImage(t *testing.T) {
	images := []string{
		"registry.example.com/some/backend:dev",
		"registry.example.com/some/redis:7.0.14-alpine",
		"registry.example.com/some/ui:dev",
	}
	if got := pickProbeImage(images); !strings.Contains(got, "redis") {
		t.Errorf("alpine-based images should be preferred, got %s", got)
	}
	if got := pickProbeImage(nil); got != "" {
		t.Errorf("an empty list should return an empty string, got %s", got)
	}
	if got := pickProbeImage([]string{"z.example/a:1"}); got != "z.example/a:1" {
		t.Errorf("without a preference hit the first image wins, got %s", got)
	}
}

func TestClassifyProbe(t *testing.T) {
	ok := true
	fail := false
	cases := []struct {
		res  probeResult
		want string
	}{
		{probeResult{}, "gateway"},
		{probeResult{GatewayOK: true}, "missing NAT"},
		{probeResult{GatewayOK: true, PublicOK: true, Endpoint: "1.2.3.4:8883"}, "firewall"},
		{probeResult{GatewayOK: true, PublicOK: true, DNSOK: &fail}, "DNS"},
		{probeResult{GatewayOK: true, PublicOK: true, DNSOK: &ok}, "normal"},
		{probeResult{GatewayOK: true, PublicOK: true, Endpoint: "1.2.3.4:8883", EndpointOK: true}, "normal"},
	}
	for _, c := range cases {
		if got := classifyProbe(&c.res); !strings.Contains(got, c.want) {
			t.Errorf("classifyProbe(%+v) = %q, want it to contain %q", c.res, got, c.want)
		}
	}
}

func TestNatDiagnosisSummaryHealthy(t *testing.T) {
	d := &natDiagnosis{IPForward: true, ForwardPolicy: "ACCEPT"}
	if !d.Healthy() {
		t.Error("no findings should be healthy")
	}
	if !strings.Contains(d.Summary(), "healthy") {
		t.Error("the healthy conclusion is missing")
	}
	d.IptablesDisabled = true
	if d.Healthy() {
		t.Error("iptables:false must be unhealthy")
	}
	if !strings.Contains(d.Summary(), "root cause") {
		t.Error("the root-cause conclusion is missing")
	}
	if len(d.FixCommands()) == 0 {
		t.Error("fix commands expected")
	}
	if len((&natDiagnosis{IPForward: true}).FixCommands()) != 0 {
		t.Error("a healthy state must not produce fix commands")
	}
}

// fakeRule answers fakeRunner.Capture for any command containing pat.
type fakeRule struct {
	pat string
	out string
}

// fakeRunner is a scripted Runner for runDoctor tests: Capture answers from
// ordered substring rules; commands containing errPat fail.
type fakeRunner struct {
	rules  []fakeRule
	errPat string
}

func (f *fakeRunner) Name() string     { return "fake" }
func (f *fakeRunner) Platform() string { return "linux" }
func (f *fakeRunner) Close() error     { return nil }
func (f *fakeRunner) MkdirAll(dir string) error {
	return nil
}
func (f *fakeRunner) UploadFile(src, dst string, log LogFunc, progress func(sent, total int64)) error {
	return nil
}
func (f *fakeRunner) WriteFile(dst string, content []byte) error { return nil }
func (f *fakeRunner) Exists(path string) (bool, error)           { return false, nil }
func (f *fakeRunner) FileMD5(path string) (string, error)        { return "", nil }
func (f *fakeRunner) Exec(ctx context.Context, out io.Writer, dir string, argv []string, env []string) error {
	return nil
}
func (f *fakeRunner) ExecStdin(ctx context.Context, out io.Writer, dir string, argv []string, env []string, stdin string) error {
	return nil
}
func (f *fakeRunner) Run(ctx context.Context, out io.Writer, dir, cmd string, env []string) error {
	return nil
}

func (f *fakeRunner) Capture(ctx context.Context, dir, cmd string, env []string) (string, error) {
	if f.errPat != "" && strings.Contains(cmd, f.errPat) {
		return "", errors.New("fake capture failure")
	}
	for _, r := range f.rules {
		if strings.Contains(cmd, r.pat) {
			return r.out, nil
		}
	}
	return "", nil
}

// healthyDoctorRules scripts a healthy docker host: one bridge network with
// its MASQUERADE rule in place, sufficient kernel quotas, docker 24 on
// cgroup v2.
func healthyDoctorRules() []fakeRule {
	return []fakeRule{
		{pat: "docker network inspect $(docker", out: `[{"Name":"bridge","Id":"abcdef0123456789","Options":{},"IPAM":{"Config":[{"Subnet":"172.17.0.0/16"}]},"Containers":{"c1":{"State":"running"}}}]`},
		{pat: "iptables -t nat -C POSTROUTING", out: "yes"},
		{pat: "sysctl -n net.ipv4.ip_forward", out: "1"},
		{pat: "iptables -S FORWARD", out: "-P FORWARD ACCEPT"},
		{pat: "sysctl -n fs.inotify.max_user_instances", out: "512"},
		{pat: "sysctl -n fs.inotify.max_user_watches", out: "1048576"},
		{pat: "sysctl -n fs.file-max", out: "131072"},
		{pat: "sysctl -n net.core.somaxconn", out: "1024"},
		{pat: "sysctl -n vm.overcommit_memory", out: "1"},
		{pat: "sysctl -n net.ipv4.ip_local_port_range", out: "32768\t60999"},
		{pat: "docker version --format", out: "24.0.7"},
		{pat: "docker info --format", out: "2"},
	}
}

func TestDoctor(t *testing.T) {
	// Healthy host: exit 0 and all four sections report.
	var b strings.Builder
	if code := runDoctor(context.Background(), &fakeRunner{rules: healthyDoctorRules()}, &b); code != 0 {
		t.Fatalf("healthy host: exit=%d, want 0\noutput:\n%s", code, b.String())
	}
	out := b.String()
	for _, want := range []string{
		"[healthy] NAT/forwarding looks good",
		"fs.inotify.max_user_instances=512",
		"fs.file-max=131072",
		"docker daemon=24.0.7 cgroup=v2",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("healthy output missing %q\noutput:\n%s", want, out)
		}
	}

	// daemon.json "iptables": false: exit 1 with the root cause and fix
	// commands.
	b.Reset()
	broken := append(healthyDoctorRules(), fakeRule{pat: "cat /etc/docker/daemon.json", out: `{"iptables": false}`})
	if code := runDoctor(context.Background(), &fakeRunner{rules: broken}, &b); code != 1 {
		t.Fatalf("iptables:false host: exit=%d, want 1\noutput:\n%s", code, b.String())
	}
	if s := b.String(); !strings.Contains(s, "root cause") || !strings.Contains(s, "Fix commands") {
		t.Errorf("broken output misses the root cause / fix commands:\n%s", s)
	}

	// The NAT probe itself fails: exit 2 (cannot diagnose).
	b.Reset()
	fr := &fakeRunner{rules: healthyDoctorRules(), errPat: "docker network inspect"}
	if code := runDoctor(context.Background(), fr, &b); code != 2 {
		t.Fatalf("NAT probe failure: exit=%d, want 2\noutput:\n%s", code, b.String())
	}
	if !strings.Contains(b.String(), "did not complete") {
		t.Errorf("expected a diagnosis-failure banner:\n%s", b.String())
	}
}
