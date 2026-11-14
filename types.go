package main

// Shared request types for the REST API (web UI and mobile clients) and the
// pinned cross-file contracts of the pack/install engines. Field names here
// are the wire format — keep them stable and additive.

// defaultAddr is the HTTP listen address used when -addr is not given.
const defaultAddr = "127.0.0.1:8765"

// Docker install policies.
const (
	DockerInstallAuto    = "auto"    // detect existing docker, else install (offline materials or online script)
	DockerInstallOffline = "offline" // always run the bundle's install materials
	DockerInstallSkip    = "skip"    // never touch docker on the target
)

// NAT fix policies for daemon.json "iptables": false targets.
const (
	NATFixRestart = "restart" // default: merge-remove the flag, restart docker
	NATFixManual  = "manual"  // add MASQUERADE rules + systemd unit, no restart
	NATFixSkip    = "skip"    // warn only
)

// PackRequest asks the packer (online machine with docker) to build a bundle.
type PackRequest struct {
	// ProjectDir is the compose project root; empty auto-detects upward from
	// the executable.
	ProjectDir string `json:"projectDir"`
	// Product selects a product from compose-installer.json; empty = default.
	Product string `json:"product"`
	// Env selects the environment (a key of the product's composeFiles).
	Env string `json:"env"`
	// Version is the major version for versioned environments; empty = latest
	// (or no version label for unversioned environments).
	Version string `json:"version"`
	// Profiles are compose profiles to pack for; empty = product defaults.
	// Selected profiles are baked into the bundle compose (trimmed, marker
	// stripped) so the installer needs no --profile flag.
	Profiles []string `json:"profiles"`

	// InstallerImage is an image carrying docker/compose offline install
	// materials under InstallerPath (default /offline, must contain install.sh).
	InstallerImage string `json:"installerImage"`
	InstallerPath  string `json:"installerPath"`

	// Output is the bundle tar.gz path; empty auto-names into <project>/dist.
	Output string `json:"output"`

	// Registry credentials for docker login on the packing machine; empty user
	// keeps the existing docker login state. Registry is required when a user
	// is given (unlike the login state, credentials are never persisted).
	Registry         string `json:"registry"`
	RegistryUser     string `json:"registryUser"`
	RegistryPassword string `json:"registryPassword"`

	// Online builds an online bundle: no images, no docker materials — the
	// target machine pulls images and installs docker over the network.
	Online bool `json:"online"`

	// Platform is the target platform, linux/amd64 or linux/arm64; empty or
	// "auto" follows the packing machine (containers are always linux).
	Platform string `json:"platform"`
}

// InstallRequest installs a bundle on the local machine or a remote SSH host.
type InstallRequest struct {
	Mode string    `json:"mode"` // local | remote
	SSH  SSHConfig `json:"ssh"`

	// BundlePath is the bundle tar.gz on the machine running the tool.
	BundlePath string `json:"bundlePath"`
	// InstallDir is the target install directory; empty = default
	// (/opt/<dirName> for root, /home/<user>/<dirName> or ~/<dirName>).
	InstallDir string `json:"installDir"`

	// DockerInstall policy (auto | offline | skip); DockerForce reinstalls
	// even when a healthy docker exists.
	DockerInstall string `json:"dockerInstall"`
	DockerForce   bool   `json:"dockerForce"`
	// InstallScript / InstallScriptArgs override the docker-offline entry
	// point (default install.sh --yes).
	InstallScript     string   `json:"installScript"`
	InstallScriptArgs []string `json:"installScriptArgs"`

	// EnvVars are user .env overrides (highest precedence; must cover every
	// manifest requiredEnv key).
	EnvVars map[string]string `json:"envVars"`

	// SudoPassword elevates on the target when the login user is non-root and
	// passwordless sudo is unavailable; empty reuses the SSH password.
	SudoPassword string `json:"sudoPassword"`

	// Registry credentials for docker login on the target (online bundles).
	Registry         string `json:"registry"`
	RegistryUser     string `json:"registryUser"`
	RegistryPassword string `json:"registryPassword"`

	// NATFixPolicy selects the daemon.json "iptables": false remedy
	// (restart | manual | skip).
	NATFixPolicy string `json:"natFixPolicy"`
}
