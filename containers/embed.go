// Package containers holds the sources of the images bh builds: the netns
// holder (egress filter) and the dev container. They are embedded so that a
// single bh binary carries everything it needs.
package containers

import "embed"

//go:embed holder dev
var FS embed.FS
