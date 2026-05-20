// Package embeddednzbget supervises a bundled NZBGet daemon so the
// abook → nzbking → NZBGet flow works without the operator running a
// separate NZBGet install. The daemon is extracted from a vendored
// tarball into the operator's existing embedded download dir, then
// supervised as a foreground subprocess we can stop cleanly on
// shutdown.
package embeddednzbget

import _ "embed"

// nzbgetBundleVersion is the NZBGet release we vendor. Bumping this
// requires regenerating the tarball at dist/. The string is exposed as
// Version() so the admin UI can render it.
const nzbgetBundleVersion = "26.1"

// nzbgetBundleSHA256 is the SHA-256 of the vendored tarball, recorded
// at vendor time. Operators (and reviewers) can verify the on-disk
// bundle matches what was checked into the repo.
const nzbgetBundleSHA256 = "503a32d6b5418086bf929d89e1debc7322d58c651c279f870a213b26837ea978"

// nzbgetBundle is the gzipped tar containing nzbget-x86_64,
// unrar-x86_64, 7za-x86_64, and cacert.pem (the cert bundle NZBGet
// uses for TLS to news providers). go:embed binds it at compile time
// so the released plugin binary contains everything it needs to run.
//
//go:embed dist/nzbget-26.1-linux-amd64.tar.gz
var nzbgetBundle []byte

// Version returns the vendored NZBGet release. The admin UI uses this
// to show "Embedded NZBGet v26.1" so operators can confirm what they're
// running without reading the plugin source.
func Version() string { return nzbgetBundleVersion }

// BundleSHA256 returns the recorded SHA-256 of the vendored tarball.
func BundleSHA256() string { return nzbgetBundleSHA256 }
