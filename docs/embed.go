// Package docs holds the operator guides and embeds them into the binary.
//
// The web interface shows exactly the guides that shipped with this build. A
// copy read from disk next to the binary would drift from the version actually
// running, and not every installation mode puts the files where the service can
// read them.
package docs

import "embed"

// Files contains every Markdown guide in this directory.
//
//go:embed *.md
var Files embed.FS
