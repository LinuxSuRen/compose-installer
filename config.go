package main

// Product configuration: turns any docker compose project into a packable,
// installable product without touching the tool itself.
//
// A project is a directory holding compose files. It may optionally carry a
// compose-installer.json at its root describing one or more products. Without
// that file the tool falls back to a zero-config single product built around
// whatever compose file it can auto-detect, so "point and pack" works for any
// plain compose project.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// ConfigFileName is the optional project descriptor read by the packer.
const ConfigFileName = "compose-installer.json"

// composeFileCandidates lists the compose file names recognized when
// auto-detecting a project (in priority order).
var composeFileCandidates = []string{
	"docker-compose.yml", "docker-compose.yaml", "compose.yaml", "compose.yml",
}

// InstallerConfig is the root of compose-installer.json.
type InstallerConfig struct {
	// DefaultProduct is chosen when a request does not name one.
	DefaultProduct string                    `json:"defaultProduct"`
	Products       map[string]*ProductConfig `json:"products"`
}

// ProductConfig describes one deployable product of a project: which compose
// file to use per environment, where version manifests live, which env keys
// the installer must collect from the user, and how the target .env is built.
//
// All relative paths are resolved against the project root, except files
// under Workdir (ComposeFiles, EnvTemplate) which are relative to Workdir.
type ProductConfig struct {
	// Workdir is the directory holding the compose file (relative to the
	// project root, "." for the root itself).
	Workdir string `json:"workdir"`

	// ComposeFiles maps environment name -> compose file name relative to
	// Workdir. The selected file is renamed to docker-compose.yml inside the
	// bundle so the target machine never deals with environment suffixes.
	ComposeFiles map[string]string `json:"composeFiles"`

	// EnvTemplate is the .env template shipped in the bundle (relative to
	// Workdir). Defaults to ".env.example"; missing file is not an error.
	EnvTemplate string `json:"envTemplate"`

	// Versions maps environment -> version manifest file (relative to the
	// project root), using the versions JSON schema:
	//
	//	{"latest": "1.0", "versions": {"1.0": {"backend": "0.0.23"}}}
	//
	// A major version selects per-component image tags; partial releases only
	// bump the components they touch. Environments without an entry use the
	// tags baked into the compose file / env template.
	Versions map[string]string `json:"versions"`

	// EnvExports are extra .env entries written at install time. Values support
	// the {version} placeholder (the selected major version), e.g.
	// "APP_VERSION": "{version}".
	EnvExports map[string]string `json:"envExports"`

	// ExtraFiles are additional files/directories (relative to Workdir) to
	// ship in the bundle, e.g. helper scripts or config directories.
	ExtraFiles []string `json:"extraFiles"`

	// DefaultProfiles preselects compose profiles when the request omits them.
	DefaultProfiles []string `json:"defaultProfiles"`

	// RequiredEnv lists env keys the installer must collect from the user
	// (validated on both web UI and backend, e.g. DEVICE_ID, JWT_SECRET).
	RequiredEnv []string `json:"requiredEnv"`

	// EnvRewrites are default .env values computed at install time, overridable
	// by the user. Values support the {installDir} placeholder, e.g.
	// "UPLOADS_HOST_PATH": "{installDir}/data/uploads" keeps state inside the
	// self-contained install directory.
	EnvRewrites map[string]string `json:"envRewrites"`

	// InstallDirName is the last path segment of the default install dir
	// (/opt/<name> for root, ~/<name> otherwise). Defaults to the product name.
	InstallDirName string `json:"installDirName"`
}

// loadInstallerConfig reads <dir>/compose-installer.json, falling back to a
// zero-config product around the auto-detected compose file. It never returns
// nil; a malformed file is an error.
func loadInstallerConfig(dir string) (*InstallerConfig, error) {
	raw, err := os.ReadFile(filepath.Join(dir, ConfigFileName))
	if os.IsNotExist(err) {
		return zeroConfig(), nil
	} else if err != nil {
		return nil, err
	}
	var c InstallerConfig
	if err := json.Unmarshal(raw, &c); err != nil {
		return nil, fmt.Errorf("parse %s: %w", ConfigFileName, err)
	}
	if len(c.Products) == 0 {
		return nil, fmt.Errorf("%s defines no products", ConfigFileName)
	}
	for name, p := range c.Products {
		p.applyDefaults(name)
	}
	return &c, nil
}

// zeroConfig builds the config used when no compose-installer.json exists:
// a single product named "default" whose "prod" environment points at the
// auto-detected compose file in the project root.
func zeroConfig() *InstallerConfig {
	p := &ProductConfig{ComposeFiles: map[string]string{"prod": ""}}
	p.applyDefaults("default")
	return &InstallerConfig{DefaultProduct: "default", Products: map[string]*ProductConfig{"default": p}}
}

// applyDefaults fills empty optional fields. name is the product key.
func (p *ProductConfig) applyDefaults(name string) {
	if p.Workdir == "" {
		p.Workdir = "."
	}
	if p.EnvTemplate == "" {
		p.EnvTemplate = ".env.example"
	}
	if p.InstallDirName == "" {
		p.InstallDirName = name
	}
}

// Product resolves a product by name; empty picks the default (or the only
// product). Unknown names list the available ones.
func (c *InstallerConfig) Product(name string) (*ProductConfig, error) {
	if name == "" {
		if c.DefaultProduct != "" {
			name = c.DefaultProduct
		} else if len(c.Products) == 1 {
			for k := range c.Products {
				name = k
			}
		}
	}
	p, ok := c.Products[name]
	if !ok {
		var names []string
		for k := range c.Products {
			names = append(names, k)
		}
		sort.Strings(names)
		return nil, fmt.Errorf("unknown product %q (available: %s)", name, strings.Join(names, ", "))
	}
	return p, nil
}

// Envs lists the environment names of the product, sorted.
func (p *ProductConfig) Envs() []string {
	envs := make([]string, 0, len(p.ComposeFiles))
	for env := range p.ComposeFiles {
		envs = append(envs, env)
	}
	sort.Strings(envs)
	return envs
}

// ComposeFileFor returns the compose file name (relative to Workdir) for env.
// Zero-config products resolve to the auto-detected file at pack time.
func (p *ProductConfig) ComposeFileFor(env string) (string, error) {
	f, ok := p.ComposeFiles[env]
	if !ok {
		return "", fmt.Errorf("environment %q is not defined for this product (available: %s)",
			env, strings.Join(p.Envs(), ", "))
	}
	if f == "" {
		// Zero-config: only "prod" with an empty mapping is allowed.
		if env == "prod" && len(p.ComposeFiles) == 1 {
			return "", nil
		}
		return "", fmt.Errorf("compose file for environment %q is empty", env)
	}
	return f, nil
}

// VersionFileFor returns the version manifest for env (relative to the project
// root), or "" when the environment uses baked-in tags.
func (p *ProductConfig) VersionFileFor(env string) string {
	return p.Versions[env]
}

// HasVersions reports whether env is driven by a version manifest.
func (p *ProductConfig) HasVersions(env string) bool {
	return p.VersionFileFor(env) != ""
}

// DetectComposeFile returns the first existing candidate compose file name in
// dir, or "" when none exists.
func DetectComposeFile(dir string) string {
	for _, name := range composeFileCandidates {
		if st, err := os.Stat(filepath.Join(dir, name)); err == nil && !st.IsDir() {
			return name
		}
	}
	return ""
}

// resolveProjectDir walks up from start (default: the executable's directory)
// looking for compose-installer.json, then for a compose file — either marks a
// project root.
func resolveProjectDir(start string) (string, error) {
	if start == "" {
		if exe, err := os.Executable(); err == nil {
			start = filepath.Dir(exe)
		} else {
			start, _ = os.Getwd()
		}
	}
	abs, err := filepath.Abs(start)
	if err != nil {
		return "", err
	}
	dir := abs
	composeRoot := ""
	for {
		if _, err := os.Stat(filepath.Join(dir, ConfigFileName)); err == nil {
			return dir, nil
		}
		if composeRoot == "" && DetectComposeFile(dir) != "" {
			composeRoot = dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			if composeRoot != "" {
				return composeRoot, nil
			}
			return "", fmt.Errorf("no project root found (no %s or compose file) above %s", ConfigFileName, abs)
		}
		dir = parent
	}
}
