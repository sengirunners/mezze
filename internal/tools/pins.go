// Package tools provisions the CLIs zm runs (rclone for off-site backups,
// cloudflared for a team's own domain, SPEC 17.11) into $ZM_HOME/tools:
// pinned public releases, per OS and arch, each archive
// checked against a sha256 written in this file before anything is
// extracted. A mismatch refuses; nothing is ever installed with sudo or a
// package manager. SPEC.md 17.2 "Tool provisioning"; the bump procedure is in
// DESIGN-NOTES.md ("Bumping the pins", under "Trackers").
package tools

import "runtime"

// Tool names.
const (
	Rclone      = "rclone" // off-site home backups (SPEC 17.9 "Off-site home backups")
	Cloudflared = "cloudflared"
)

// Pin is one release archive of one tool for one platform.
type Pin struct {
	Tool    string // Rclone or Cloudflared
	Version string // "1.75.2", as the tool prints it
	OS      string // runtime.GOOS
	Arch    string // runtime.GOARCH
	URL     string // the release asset on github.com
	SHA256  string // of the archive (the release's own digest)
	Member  string // the executable's path inside the archive (.tar.gz, or .zip when URL ends so); "": the download is the executable
}

// CloudflaredVersion is cloudflare/cloudflared 2026.10.7 (2026-10-30), for
// zm tunnel setup (SPEC 17.11). The pins are GitHub's own asset digests. The
// release notes' sums for the two macOS entries are of the executable inside
// each archive (checked: darwin-arm64's binary is 2136431d...bb12, signed
// Developer ID Application: Cloudflare Inc., 68WVV388M8); the Linux ones are
// the downloads themselves, which are bare executables.
const CloudflaredVersion = "2026.10.7"

const cfBase = "https://github.com/cloudflare/cloudflared/releases/download/" + CloudflaredVersion + "/"

// RcloneVersion is rclone v1.75.2 (rclone/rclone, 2026-10-09). The digests
// are the release's SHA256SUMS. rclone uploads the off-site home backups
// (S3, R2 and the rest of its backends; its crypt backend encrypts them).
const (
	RcloneVersion = "1.75.2"
	rcloneBase    = "https://github.com/rclone/rclone/releases/download/v" + RcloneVersion + "/rclone-v" + RcloneVersion + "-"
)

// Pins is every pinned archive. Tests may replace it.
var Pins = []Pin{
	{Rclone, RcloneVersion, "darwin", "arm64", rcloneBase + "osx-arm64.zip", "610ab7d48615135dfc60b763858b11a426a0d3d1160c1bb76fb0ae6011b02edd", "rclone-v1.75.2-osx-arm64/rclone"},
	{Rclone, RcloneVersion, "darwin", "amd64", rcloneBase + "osx-amd64.zip", "574e1f31cf1c464e5fb0325cb66fcbdeaf1efa1802bd9b17f13c256fd74b6561", "rclone-v1.75.2-osx-amd64/rclone"},
	{Rclone, RcloneVersion, "linux", "arm64", rcloneBase + "linux-arm64.zip", "7e1e8d69654941b7b7df84ee74c5f7fb09cce7d5496947fb7bf16b55ff427d10", "rclone-v1.75.2-linux-arm64/rclone"},
	{Rclone, RcloneVersion, "linux", "amd64", rcloneBase + "linux-amd64.zip", "349ac8fba6ff65d6247043f1750cdcb518ec5d500ef91463a10d37c0ccdf3702", "rclone-v1.75.2-linux-amd64/rclone"},
	{Cloudflared, CloudflaredVersion, "darwin", "arm64", cfBase + "cloudflared-darwin-arm64.tgz", "3586ccf397e98e5f437e60e7c307ca6ac463e110580503af8bb8e6f9ebf831bd", "cloudflared"},
	{Cloudflared, CloudflaredVersion, "darwin", "amd64", cfBase + "cloudflared-darwin-amd64.tgz", "02f3edfd87684dd5b705607688e4eb4b323f6e777b81e7b3a85af556aa356848", "cloudflared"},
	{Cloudflared, CloudflaredVersion, "linux", "arm64", cfBase + "cloudflared-linux-arm64", "3df43aedc9ff291d1735b5cb576cb97e0b157488796db2e32b28aecabb445775", ""},
	{Cloudflared, CloudflaredVersion, "linux", "amd64", cfBase + "cloudflared-linux-amd64", "1f99fb518b83dec2ba8d870a7cc991d82a77e6196295a3d2754ba17ab9f91872", ""},
}

// PinFor is the pin of tool for this machine's OS and arch.
func PinFor(tool string) (Pin, bool) { return pinFor(tool, runtime.GOOS, runtime.GOARCH) }

func pinFor(tool, goos, goarch string) (Pin, bool) {
	for _, p := range Pins {
		if p.Tool == tool && p.OS == goos && p.Arch == goarch {
			return p, true
		}
	}
	return Pin{}, false
}
