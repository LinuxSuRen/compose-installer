// Host diagnostics and self-healing for docker compose deployments.
//
// The doctor subcommand reports host-level failure classes read-only with
// root causes and ready-to-run fix commands; the install flow calls the
// fix*/ensure* helpers to apply the same remedies automatically. This file
// owns the container egress (NAT) segment and the docker version / cgroup
// check; the inotify quota segment lives in inotify.go and the kernel quota
// family (file-max / somaxconn / overcommit / port range) in sysctls.go;
// runDoctor wires all four segments together.
package main

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ---------------- container egress (NAT) ----------------

// daemonJSONPath is the docker daemon config path (Ubuntu convention).
const daemonJSONPath = "/etc/docker/daemon.json"

// bridgeNet holds the NAT-relevant facts of one docker bridge network
// (everything is discovered dynamically; never hardcode subnets).
type bridgeNet struct {
	Name    string // network name (the default bridge is "bridge")
	Subnet  string // e.g. 172.18.0.0/16; empty = unassigned (no containers attached)
	Bridge  string // bridge interface: Options override, or docker0 for the default bridge, or br-<first 12 id chars>
	Running int    // number of running containers attached
}

// natDiagnosis is the result of one full diagnosis pass.
type natDiagnosis struct {
	DaemonJSONExists   bool
	IptablesDisabled   bool     // daemon.json explicitly sets "iptables": false
	IPForward          bool     // net.ipv4.ip_forward
	ForwardPolicy      string   // FORWARD chain policy
	ForwardDropForNets []string // subnets affected when the FORWARD chain has no ACCEPT path
	Nets               []bridgeNet
	MissingMASQ        []string // subnets lacking a MASQUERADE rule (with bridge names)
}

// daemonIptablesDisabled reports whether daemon.json explicitly disables
// iptables. Malformed JSON returns false and is left to the caller to flag
// for manual inspection.
func daemonIptablesDisabled(raw string) bool {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return false
	}
	var cfg map[string]any
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		return false
	}
	v, ok := cfg["iptables"]
	if !ok {
		return false
	}
	b, isBool := v.(bool)
	return isBool && !b
}

// stripIptablesFlag removes the "iptables" key via a JSON merge and returns
// the new content; every other setting (data-root, registry-mirrors,
// runtimes, ...) is preserved. When the key is absent the input is returned
// unchanged (idempotent), changed=false.
func stripIptablesFlag(raw string) (out string, changed bool, err error) {
	var cfg map[string]any
	if err = json.Unmarshal([]byte(raw), &cfg); err != nil {
		return "", false, fmt.Errorf("daemon.json is not valid JSON, refusing to modify it: %w", err)
	}
	v, ok := cfg["iptables"]
	if !ok {
		return raw, false, nil
	}
	if b, isBool := v.(bool); !isBool || b {
		return raw, false, nil // iptables:true needs no treatment
	}
	delete(cfg, "iptables")
	b, merr := json.MarshalIndent(cfg, "", "  ")
	if merr != nil {
		return "", false, merr
	}
	return string(b) + "\n", true, nil
}

// inspectBridgeNets dynamically enumerates every bridge network with its
// subnet and bridge interface (parsing the raw docker network inspect JSON
// instead of stitching a fragile --format in shell).
func inspectBridgeNets(ctx context.Context, runner Runner) ([]bridgeNet, error) {
	out, err := runner.Capture(ctx, "",
		`docker network inspect $(docker network ls -q --filter driver=bridge) 2>/dev/null || true`, nil)
	if err != nil {
		return nil, fmt.Errorf("docker network inspect failed: %w", err)
	}
	if strings.TrimSpace(out) == "" {
		return nil, nil // no bridge networks (e.g. docker freshly installed)
	}
	var nets []struct {
		Name    string            `json:"Name"`
		Id      string            `json:"Id"`
		Options map[string]string `json:"Options"`
		IPAM    struct {
			Config []struct {
				Subnet string `json:"Subnet"`
			} `json:"Config"`
		} `json:"IPAM"`
		Containers map[string]struct {
			State string `json:"State"`
		} `json:"Containers"`
	}
	if err := json.Unmarshal([]byte(out), &nets); err != nil {
		return nil, fmt.Errorf("failed to parse network inspect output: %w", err)
	}
	var res []bridgeNet
	for _, n := range nets {
		bn := bridgeNet{Name: n.Name}
		if len(n.IPAM.Config) > 0 {
			bn.Subnet = n.IPAM.Config[0].Subnet
		}
		switch {
		case n.Options["com.docker.network.bridge.name"] != "":
			bn.Bridge = n.Options["com.docker.network.bridge.name"]
		case n.Name == "bridge":
			bn.Bridge = "docker0"
		default:
			bn.Bridge = "br-" + n.Id[:12]
		}
		for _, c := range n.Containers {
			if c.State == "running" {
				bn.Running++
			}
		}
		res = append(res, bn)
	}
	return res, nil
}

// masqRuleFor returns the MASQUERADE rule of a subnet (matching the rule
// verified manually during the incident: source in the subnet and outbound
// interface other than this bridge means SNAT).
func masqRuleFor(subnet, bridge string) string {
	return fmt.Sprintf("-s %s ! -o %s -j MASQUERADE", subnet, bridge)
}

// diagnoseNAT runs the full diagnosis: the daemon.json switch, ip_forward,
// the FORWARD chain, and the MASQUERADE presence of every bridge subnet.
// Read-only, safe to repeat.
//
// Field-proven background: Jetson edge devices where containers got i/o
// timeouts talking to an MQTT broker while the host worked fine. Root cause:
// dockerd wrote no MASQUERADE rules, so container traffic left the NIC with
// a private source IP and no reply ever came back.
func diagnoseNAT(ctx context.Context, runner Runner) (*natDiagnosis, error) {
	d := &natDiagnosis{}
	if raw, err := runner.Capture(ctx, "", `cat `+daemonJSONPath+` 2>/dev/null || true`, nil); err == nil {
		raw = strings.TrimSpace(raw)
		d.DaemonJSONExists = raw != ""
		d.IptablesDisabled = daemonIptablesDisabled(raw)
	}
	if out, err := runner.Capture(ctx, "", `sysctl -n net.ipv4.ip_forward 2>/dev/null`, nil); err == nil {
		d.IPForward = strings.TrimSpace(out) == "1"
	}
	if out, err := runner.Capture(ctx, "", `iptables -S FORWARD 2>/dev/null | head -1`, nil); err == nil {
		// shaped like "-P FORWARD ACCEPT"
		if f := strings.Fields(strings.TrimSpace(out)); len(f) >= 3 {
			d.ForwardPolicy = strings.Trim(strings.Join(f[2:], " "), "[]")
		}
	}
	nets, err := inspectBridgeNets(ctx, runner)
	if err != nil {
		return nil, err
	}
	d.Nets = nets
	for _, n := range nets {
		if n.Subnet == "" {
			continue
		}
		chk, cerr := runner.Capture(ctx, "", fmt.Sprintf(
			`iptables -t nat -C POSTROUTING %s 2>/dev/null && echo yes || echo no`, masqRuleFor(n.Subnet, n.Bridge)), nil)
		if cerr == nil && strings.TrimSpace(chk) != "yes" {
			d.MissingMASQ = append(d.MissingMASQ, fmt.Sprintf("%s (%s via %s)", n.Subnet, n.Name, n.Bridge))
		}
		// Record subnets with no ACCEPT path when the FORWARD policy is DROP
		// (policy ACCEPT, or a DOCKER chain jump as left by a default
		// installation, is not a problem).
		if d.ForwardPolicy == "DROP" {
			chk2, _ := runner.Capture(ctx, "", fmt.Sprintf(
				`iptables -S FORWARD 2>/dev/null | grep -c -- '-s %s.*ACCEPT'`, n.Subnet), nil)
			if c, e := strconv.Atoi(strings.TrimSpace(chk2)); e == nil && c == 0 {
				d.ForwardDropForNets = append(d.ForwardDropForNets, n.Subnet)
			}
		}
	}
	return d, nil
}

// Healthy reports whether the diagnosis found nothing to fix.
func (d *natDiagnosis) Healthy() bool {
	return !d.IptablesDisabled && len(d.MissingMASQ) == 0 && len(d.ForwardDropForNets) == 0
}

// Summary is the human-readable conclusion (shared by doctor output and
// install logs).
func (d *natDiagnosis) Summary() string {
	var b strings.Builder
	if d.IptablesDisabled {
		fmt.Fprintln(&b, "[root cause] /etc/docker/daemon.json sets \"iptables\": false - dockerd writes no NAT rules,")
		fmt.Fprintln(&b, "             container traffic leaves the NIC with a private source IP and no reply comes back:")
		fmt.Fprintln(&b, "             containers cannot reach the public internet while the host itself works fine.")
	} else if len(d.MissingMASQ) > 0 {
		fmt.Fprintln(&b, "[issue] these subnets have no MASQUERADE (SNAT) rule:")
		for _, m := range d.MissingMASQ {
			fmt.Fprintf(&b, "         - %s\n", m)
		}
	}
	if !d.IPForward {
		fmt.Fprintln(&b, "[issue] net.ipv4.ip_forward=0: container traffic is not forwarded.")
	}
	if len(d.ForwardDropForNets) > 0 {
		fmt.Fprintf(&b, "[issue] FORWARD chain policy DROP without an ACCEPT path: %s\n", strings.Join(d.ForwardDropForNets, ", "))
	}
	if d.Healthy() {
		fmt.Fprintln(&b, "[healthy] NAT/forwarding looks good: no iptables:false in daemon.json, every bridge subnet has a MASQUERADE rule, forwarding enabled.")
	}
	return strings.TrimRight(b.String(), "\n")
}

// FixCommands returns ready-to-run fix commands for the current diagnosis
// (used by doctor output). Policy values live in types.go (NATFixRestart /
// NATFixManual / NATFixSkip); the installer picks one, humans get both.
func (d *natDiagnosis) FixCommands() []string {
	if d.Healthy() {
		return nil
	}
	var cmds []string
	if d.IptablesDisabled {
		cmds = append(cmds,
			"# Option A (recommended): remove iptables:false and restart docker (affects all running containers)",
			"sudo python3 -c 'import json;p=\"/etc/docker/daemon.json\";c=json.load(open(p));c.pop(\"iptables\",None);json.dump(c,open(p,\"w\"),indent=2)' && sudo systemctl restart docker",
			"# Option B (no restart): add MASQUERADE for every bridge subnet (subnet/bridge discovered via docker network inspect)",
			"for n in $(docker network ls --filter driver=bridge --format '{{.Name}}'); do",
			"  eval $(docker network inspect $n --format '{{range .IPAM.Config}}SUBNET={{.Subnet}}{{end}}')",
			"  BR=$(docker network inspect $n --format '{{index .Options \"com.docker.network.bridge.name\"}}'); [ -n \"$BR\" ] || BR=br-$(docker network inspect $n --format '{{.Id}}' | cut -c1-12)",
			"  sudo iptables -t nat -C POSTROUTING -s $SUBNET ! -o $BR -j MASQUERADE 2>/dev/null || sudo iptables -t nat -A POSTROUTING -s $SUBNET ! -o $BR -j MASQUERADE",
			"done")
	} else if len(d.MissingMASQ) > 0 {
		for _, n := range d.Nets {
			if n.Subnet == "" {
				continue
			}
			cmds = append(cmds, fmt.Sprintf(
				"sudo iptables -t nat -C POSTROUTING %s 2>/dev/null || sudo iptables -t nat -A POSTROUTING %s",
				masqRuleFor(n.Subnet, n.Bridge), masqRuleFor(n.Subnet, n.Bridge)))
		}
	}
	if !d.IPForward {
		cmds = append(cmds, "sudo sysctl -w net.ipv4.ip_forward=1 && echo net.ipv4.ip_forward=1 | sudo tee /etc/sysctl.d/99-compose-installer.conf")
	}
	return cmds
}

// natApplyScript is the boot-time self-healing script of option B: it
// re-enumerates the bridge networks on every boot and re-adds the rules
// (nothing to maintain when networks come and go), idempotent via -C.
// The default bridge network is included too (subnet 172.17.0.0/16, bridge
// docker0).
const natApplyScript = `#!/bin/sh
# Generated by compose-installer: add MASQUERADE rules for all docker bridge networks (idempotent).
# Background: with daemon.json "iptables": false dockerd writes no NAT rules and containers
# cannot reach the public internet. Subnets and bridge names are taken dynamically from
# docker network inspect - nothing is hardcoded.
set -eu
docker network ls --filter driver=bridge --format '{{.Name}}' 2>/dev/null | while read -r n; do
  info="$(docker network inspect "$n" --format '{{range .IPAM.Config}}{{.Subnet}}{{end}} {{.Id}} {{index .Options "com.docker.network.bridge.name"}}' 2>/dev/null || true)"
  subnet="$(echo "$info" | awk '{print $1}')"
  id="$(echo "$info" | awk '{print $2}')"
  br="$(echo "$info" | awk '{print $3}')"
  [ -n "$subnet" ] || continue
  if [ -z "$br" ]; then
    if [ "$n" = "bridge" ]; then br=docker0; else br="br-$(echo "$id" | cut -c1-12)"; fi
  fi
  iptables -t nat -C POSTROUTING -s "$subnet" ! -o "$br" -j MASQUERADE 2>/dev/null || \
    iptables -t nat -A POSTROUTING -s "$subnet" ! -o "$br" -j MASQUERADE
done
`

const natUnit = `[Unit]
Description=compose-installer container NAT (MASQUERADE) rules for bridge networks
After=docker.service
Wants=docker.service

[Service]
Type=oneshot
ExecStart=/usr/local/sbin/compose-installer-nat-apply.sh
RemainAfterExit=yes

[Install]
WantedBy=multi-user.target
`

func sha256Hex(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// runOut executes a command and returns its combined output (a convenience
// wrapper around runner.Run).
func runOut(ctx context.Context, runner Runner, cmd string) (string, error) {
	var b strings.Builder
	if err := runner.Run(ctx, &b, "", cmd, nil); err != nil {
		return b.String(), err
	}
	return b.String(), nil
}

// writeRootFile writes a root-owned file on the target via sudo (base64 to
// sidestep quoting issues); idempotent: skipped when the content matches.
// Shared by the NAT fix paths here and by inotify.go / sysctls.go.
func writeRootFile(ctx context.Context, runner Runner, sudo, path, content string) error {
	sum, err := runner.Capture(ctx, "", fmt.Sprintf(`sha256sum %s 2>/dev/null | cut -d' ' -f1`, shQuote(path)), nil)
	if err == nil && strings.TrimSpace(sum) == sha256Hex(content) {
		return nil // already present with identical content
	}
	b64 := base64.StdEncoding.EncodeToString([]byte(content))
	return runner.Run(ctx, io.Discard, "", fmt.Sprintf(
		`printf %%s %s | base64 -d | %s tee %s >/dev/null`, shQuote(b64), sudo, shQuote(path)), nil)
}

// ensureNATRulesManual is option B: immediately add the missing MASQUERADE
// rules for every bridge subnet and persist them (a systemd oneshot unit
// recomputes them at boot, so removed docker networks never leave stale
// rules behind).
func ensureNATRulesManual(ctx context.Context, runner Runner, j *Job, sudo string) error {
	nets, err := inspectBridgeNets(ctx, runner)
	if err != nil {
		return err
	}
	applied := 0
	for _, n := range nets {
		if n.Subnet == "" {
			continue
		}
		chk, cerr := runner.Capture(ctx, "", fmt.Sprintf(
			`iptables -t nat -C POSTROUTING %s 2>/dev/null && echo yes || echo no`, masqRuleFor(n.Subnet, n.Bridge)), nil)
		if cerr != nil && strings.Contains(cerr.Error(), "docker") {
			return cerr
		}
		if cerr == nil && strings.TrimSpace(chk) == "yes" {
			continue
		}
		if _, rerr := runOut(ctx, runner, fmt.Sprintf(
			`%s iptables -t nat -A POSTROUTING %s`, sudo, masqRuleFor(n.Subnet, n.Bridge))); rerr != nil {
			return fmt.Errorf("failed to add MASQUERADE rule (%s): %v", n.Subnet, rerr)
		}
		applied++
		j.Logf("added MASQUERADE: %s (%s via %s)", n.Subnet, n.Name, n.Bridge)
	}
	if applied == 0 {
		j.Logf("MASQUERADE rules already complete (%d bridge networks), nothing to add", len(nets))
	}
	// Persist: script + unit + enable (all idempotent)
	if err := writeRootFile(ctx, runner, sudo, "/usr/local/sbin/compose-installer-nat-apply.sh", natApplyScript+"\n"); err != nil {
		return fmt.Errorf("failed to write the NAT self-healing script: %w", err)
	}
	if err := writeRootFile(ctx, runner, sudo, "/etc/systemd/system/compose-installer-nat.service", natUnit); err != nil {
		return fmt.Errorf("failed to write the systemd unit: %w", err)
	}
	if out, err := runOut(ctx, runner, fmt.Sprintf(
		`%s chmod +x /usr/local/sbin/compose-installer-nat-apply.sh && %s systemctl daemon-reload && (%s systemctl enable compose-installer-nat.service >/dev/null 2>&1 || true) && echo ok`, sudo, sudo, sudo)); err != nil || strings.TrimSpace(out) != "ok" {
		return fmt.Errorf("failed to enable compose-installer-nat service: %v %s", err, out)
	}
	j.Logf("NAT rules persisted (compose-installer-nat.service, recomputed at every boot)")
	return nil
}

// fixDaemonJSONRestart is option A: backup -> JSON-merge-remove iptables:false
// -> restart docker -> wait until ready. Idempotent: a no-op when the key is
// absent.
func fixDaemonJSONRestart(ctx context.Context, runner Runner, j *Job, sudo string) error {
	raw, err := runner.Capture(ctx, "", `cat `+daemonJSONPath+` 2>/dev/null || true`, nil)
	if err != nil {
		return err
	}
	raw = strings.TrimSpace(raw)
	if raw == "" || !daemonIptablesDisabled(raw) {
		j.Logf("daemon.json has no \"iptables\": false, nothing to do")
		return nil
	}
	out, changed, err := stripIptablesFlag(raw)
	if err != nil || !changed {
		return err
	}
	j.Logf("warning: about to modify %s (removing \"iptables\": false) and restart docker,", daemonJSONPath)
	j.Logf("         the restart affects every running container on the target (business containers come back with the install flow)")
	// Backup (-n never overwrites an existing backup, the original config stays on disk)
	if _, berr := runOut(ctx, runner, fmt.Sprintf(`%s cp -n %s %s.compose-installer-bak`, sudo, daemonJSONPath, daemonJSONPath)); berr != nil {
		return fmt.Errorf("failed to back up daemon.json: %v", berr)
	}
	if err := writeRootFile(ctx, runner, sudo, daemonJSONPath, out); err != nil {
		return fmt.Errorf("failed to write daemon.json back: %w", err)
	}
	j.Logf("daemon.json updated via JSON merge (other settings preserved, backup: %s.compose-installer-bak)", daemonJSONPath)
	if _, rerr := runOut(ctx, runner, fmt.Sprintf(`%s systemctl restart docker`, sudo)); rerr != nil {
		return fmt.Errorf("failed to restart docker: %v (check journalctl -u docker)", rerr)
	}
	j.Logf("docker restarted, waiting for it to become ready...")
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if out, cerr := runner.Capture(ctx, "", `docker info >/dev/null 2>&1 && echo ok`, nil); cerr == nil && strings.TrimSpace(out) == "ok" {
			j.Logf("docker is ready")
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(3 * time.Second):
		}
	}
	return fmt.Errorf("docker did not become ready within 60s after the restart")
}

// ---------------- container egress probe ----------------

// probeResult is the connectivity probe result collected inside a container
// (a single key-value line protocol, to avoid line-by-line parsing).
type probeResult struct {
	GatewayOK  bool
	PublicOK   bool
	Endpoint   string // host:port (may be empty)
	EndpointOK bool
	DNSOK      *bool // nil = not probed
}

// pickProbeImage chooses the probe vehicle from the images loaded from the
// bundle (alpine-based images with busybox tools first).
func pickProbeImage(images []string) string {
	pref := []string{"redis", "nginx", "eclipse-mosquitto", "mediamtx", "busybox", "alpine", "cadvisor", "prometheus"}
	for _, p := range pref {
		for _, img := range images {
			if strings.Contains(img, p) {
				return img
			}
		}
	}
	if len(images) > 0 {
		sort.Strings(images)
		return images[0]
	}
	return ""
}

// endpointRe requires the endpoint to look like host:port (host being an
// IP/domain) so user input cannot inject into the probe script.
var endpointRe = regexp.MustCompile(`^[A-Za-z0-9._-]+:[0-9]{1,5}$`)

// probeContainerEgress starts a throwaway container on the compose network
// and probes connectivity: gateway -> public internet -> business endpoint
// (TCP) -> DNS. The probe image comes from the bundle (nothing to pull in an
// offline environment).
func probeContainerEgress(ctx context.Context, runner Runner, j *Job, network, probeImage, endpoint string) (*probeResult, error) {
	if endpoint != "" && !endpointRe.MatchString(endpoint) {
		j.Logf("warning: business endpoint %q is malformed (expected host:port), skipping the endpoint probe", endpoint)
		endpoint = ""
	}
	script := `GW=$(ip route | awk '/default/{print $3; exit}')
ping -c1 -W2 $GW >/dev/null 2>&1 && echo GW:ok || echo GW:fail
ping -c1 -W2 223.5.5.5 >/dev/null 2>&1 && echo PUB:ok || echo PUB:fail`
	if endpoint != "" {
		script += `
H=` + shQuote(strings.SplitN(endpoint, ":", 2)[0]) + `; P=` + shQuote(strings.SplitN(endpoint, ":", 2)[1]) + `
nc -z -w3 $H $P >/dev/null 2>&1 && echo EP:ok || echo EP:fail`
	}
	script += `
nslookup baidu.com >/dev/null 2>&1 && echo DNS:ok || echo DNS:fail`
	out, err := runner.Capture(ctx, "", fmt.Sprintf(
		`docker run --rm --network %s --entrypoint sh %s -c %s 2>&1 || true`,
		shQuote(network), shQuote(probeImage), shQuote(script)), nil)
	if err != nil {
		return nil, err
	}
	res := &probeResult{Endpoint: endpoint}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		switch {
		case line == "GW:ok":
			res.GatewayOK = true
		case line == "PUB:ok":
			res.PublicOK = true
		case line == "EP:ok":
			res.EndpointOK = true
		case line == "DNS:ok":
			v := true
			res.DNSOK = &v
		case line == "DNS:fail":
			v := false
			res.DNSOK = &v
		}
	}
	return res, nil
}

// ---------------- doctor subcommand ----------------

// runDoctor is the doctor subcommand body: diagnose -> conclusion -> fix
// commands; returns the exit code (0 healthy / 1 issues found / 2 could not
// diagnose). It covers the host-level failure classes: container egress
// (NAT), kernel quotas (inotify and the quota family) and the docker
// daemon version.
func runDoctor(ctx context.Context, runner Runner, w io.Writer) int {
	exit := 0
	fmt.Fprintln(w, "== compose-installer doctor: host diagnostics (NAT / kernel quotas / docker) ==")
	d, err := diagnoseNAT(ctx, runner)
	if err != nil {
		fmt.Fprintf(w, "[error] NAT diagnosis did not complete: %v\n(this command must run on the docker host)\n", err)
		exit = 2
	} else {
		fmt.Fprintf(w, "daemon.json exists=%v iptables:false=%v ip_forward=%v FORWARD policy=%s\n",
			d.DaemonJSONExists, d.IptablesDisabled, d.IPForward, d.ForwardPolicy)
		for _, n := range d.Nets {
			fmt.Fprintf(w, "  network %-24s subnet=%-18s bridge=%-16s running=%d\n", n.Name, n.Subnet, n.Bridge, n.Running)
		}
		fmt.Fprintln(w)
		fmt.Fprintln(w, d.Summary())
		exit = doctorPrintFixes(w, d.FixCommands(), exit)
	}

	fmt.Fprintln(w)
	id, ierr := diagnoseInotify(ctx, runner)
	if ierr != nil {
		fmt.Fprintf(w, "[error] inotify diagnosis did not complete: %v\n", ierr)
		exit = 2
	} else {
		fmt.Fprintln(w, id.Summary())
		exit = doctorPrintFixes(w, id.FixCommands(), exit)
	}

	// Kernel quota family (file-max/somaxconn are fixable; overcommit and
	// the port range are advisory only).
	fmt.Fprintln(w)
	kd, kerr := diagnoseKernelQuotas(ctx, runner)
	if kerr != nil {
		fmt.Fprintf(w, "[error] kernel quota diagnosis did not complete: %v\n", kerr)
		exit = 2
	} else {
		fmt.Fprintln(w, kd.Summary())
		exit = doctorPrintFixes(w, kd.FixCommands(), exit)
	}

	// docker version / cgroup mode: an old daemon (< 20.10) cannot start
	// containers on a cgroup v2 host (the default on Ubuntu 22.04+,
	// Debian 11+, RHEL 9).
	fmt.Fprintln(w)
	ver, verr := runner.Capture(ctx, "", `docker version --format '{{.Server.Version}}' 2>/dev/null`, nil)
	if verr != nil || strings.TrimSpace(ver) == "" {
		fmt.Fprintln(w, "docker=unavailable (not installed / daemon not running / current user lacks docker permission)")
	} else {
		cg, _ := runner.Capture(ctx, "", `docker info --format '{{.CgroupVersion}}' 2>/dev/null`, nil)
		ver = strings.TrimSpace(ver)
		fmt.Fprintf(w, "docker daemon=%s cgroup=v%s\n", ver, strings.TrimSpace(cg))
		if !dockerVersionAtLeast(ver, dockerMinMajor, dockerMinMinor) {
			fmt.Fprintf(w, "[issue] docker %s is below the minimum requirement %d.%d: containers cannot start on cgroup v2 hosts,\n", ver, dockerMinMajor, dockerMinMinor)
			fmt.Fprintln(w, "         and several newer features (compose v2, health-check integration) are unavailable as well.")
			exit = doctorPrintFixes(w, []string{
				"# Option A: reinstall with compose-installer (keep the docker install policy on auto; an old daemon is replaced by the bundle materials automatically)",
				"# Option B: upgrade online on the target: curl -fsSL https://get.docker.com | sudo sh",
			}, exit)
		}
	}
	return exit
}

// doctorPrintFixes prints the fix command list and lifts the exit code to 1
// when there is something to fix (a higher-priority 2 is kept as is).
func doctorPrintFixes(w io.Writer, cmds []string, exit int) int {
	if len(cmds) == 0 {
		return exit
	}
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Fix commands (run them directly, or let the installer apply them by policy):")
	for _, c := range cmds {
		fmt.Fprintf(w, "  %s\n", c)
	}
	if exit == 0 {
		return 1
	}
	return exit
}

// ---------------- install-flow helpers ----------------

// composeNetworkName finds the network of a compose project by matching the
// label com.docker.compose.project (the compose project name is the compose
// file's directory name, e.g. repo/<workdir> in the install layout).
func composeNetworkName(ctx context.Context, runner Runner, project string) (string, error) {
	out, err := runner.Capture(ctx, "", fmt.Sprintf(
		`docker network ls --filter label=com.docker.compose.project=%s --format '{{.Name}}' | head -1`, shQuote(project)), nil)
	if err != nil {
		return "", err
	}
	name := strings.TrimSpace(out)
	if name == "" {
		return "", fmt.Errorf("no network found for compose project %s (are the containers up?)", project)
	}
	return name, nil
}

// classifyProbe turns a probe result into an actionable conclusion (the
// three failure classes plus gateway trouble plus healthy).
func classifyProbe(res *probeResult) string {
	switch {
	case !res.GatewayOK:
		return "container cannot even reach its own gateway: bridge/subnet or container network misconfiguration (not a NAT problem); run compose-installer doctor on the target and inspect docker network inspect"
	case res.GatewayOK && !res.PublicOK:
		return "container cannot reach the public internet (missing NAT): the host forwards the traffic but the source IP is never SNATed. Run compose-installer doctor to locate the cause (typical root cause: daemon.json \"iptables\": false)"
	case res.PublicOK && res.Endpoint != "" && !res.EndpointOK:
		return fmt.Sprintf("container reaches the public internet but the business endpoint %s is unreachable: usually the peer firewall/allowlist does not permit this machine's egress IP, or the endpoint service is not listening (verify with curl -v / telnet on the target)", res.Endpoint)
	case res.DNSOK != nil && !*res.DNSOK && res.PublicOK:
		return "DNS fails while the public IP is reachable: check the container /etc/resolv.conf or the LAN DNS server"
	default:
		return "container network connectivity is normal (gateway/public/DNS" + map[bool]string{true: "/business endpoint"}[res.Endpoint != ""] + " all reachable)"
	}
}
