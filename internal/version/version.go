// Package version holds the build-time version string shared by every
// binary in this repo (nexus, agentfunnel, nexus-comms-mcp, nexus-imap-mcp,
// nexus-jira-mcp, nexus-watch, aspect, outpost, etc).
//
// Default is "dev"; release builds override via -ldflags:
//
//	go build -ldflags "-X github.com/CarriedWorldUniverse/nexus/internal/version.Version=v0.1.0" ./...
//
// goreleaser handles the injection at release time; the Makefile does
// it via `git describe --tags --always --dirty` for local dev builds.
package version

// Version is the build-time version string. Overridden via -ldflags;
// "dev" when unset.
var Version = "dev"
