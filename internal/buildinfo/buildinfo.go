// Package buildinfo identifies the source revision compiled into a binary.
package buildinfo

// Commit is set by Docker builds through the Go linker.
var Commit = "unknown"
