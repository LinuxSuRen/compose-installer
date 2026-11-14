package main

// Bundle format: a pack is a single tar.gz whose root contains
// manifest.json plus the payload directories below. The manifest is the only
// contract between the packer and the installer (and the mobile clients) —
// keep it additive and bump BundleSchemaVersion on breaking changes.
//
//	manifest.json          this file
//	repo/                  the compose project subset (compose renamed to
//	                       docker-compose.yml, env template, extra files…)
//	images/<product>.tar   docker save of all images (offline bundles only)
//	docker-offline/        docker/compose offline install materials extracted
//	                       from the installer image (offline bundles only)

import "time"

// BundleSchemaVersion is the manifest schema version. 1 is the initial
// compose-installer format.
const BundleSchemaVersion = 1

// BundleManifest is written to the bundle root and read by the installer.
type BundleManifest struct {
	SchemaVersion int       `json:"schemaVersion"`
	Created       time.Time `json:"created"`

	// Product is the product name from compose-installer.json ("default" for
	// zero-config projects).
	Product string `json:"product"`
	// Env is the selected environment name (e.g. prod, test).
	Env string `json:"env"`
	// Version is the selected major version ("dev"-like free labels allowed;
	// empty when the environment uses baked-in tags).
	Version string `json:"version"`

	// ComposeFile is the compose file relative to repo/<workdir>; always the
	// normalized "docker-compose.yml" for bundles of schema 1.
	ComposeFile string `json:"composeFile"`
	// Workdir is the compose working directory relative to repo/ ("." or a
	// subdirectory such as "robot").
	Workdir string `json:"workdir"`
	// Profiles are the compose profiles the bundle was trimmed for (empty once
	// trimmed: the bundle compose is self-contained).
	Profiles []string `json:"profiles"`

	// Images lists every image reference (offline: saved into images/;
	// online: pulled by the target machine).
	Images []string `json:"images"`
	// Components maps component -> tag for versioned products.
	Components map[string]string `json:"components,omitempty"`

	// Online marks a bundle without images and docker materials: the target
	// machine pulls images and (if needed) installs docker from the network.
	Online bool `json:"online,omitempty"`

	// Platform is the target platform (linux/amd64|linux/arm64). Offline
	// bundles abort on mismatch with the target machine; online bundles only
	// warn. Empty = legacy bundle, no check.
	Platform string `json:"platform,omitempty"`

	// InstallDirName is the last path segment of the default install directory
	// (/opt/<name> for root targets, ~/<name> otherwise). Defaults to Product.
	InstallDirName string `json:"installDirName,omitempty"`

	// RequiredEnv lists env keys the installer must collect (from
	// compose-installer.json; enforced by UI and backend).
	RequiredEnv []string `json:"requiredEnv,omitempty"`
	// EnvExports are extra .env entries; values may contain {version}.
	EnvExports map[string]string `json:"envExports,omitempty"`
	// EnvRewrites are install-time .env defaults; values may contain
	// {installDir}. User-provided overrides win.
	EnvRewrites map[string]string `json:"envRewrites,omitempty"`
}

// DirName returns the default install directory name for this bundle.
func (m *BundleManifest) DirName() string {
	if m.InstallDirName != "" {
		return m.InstallDirName
	}
	return m.Product
}
