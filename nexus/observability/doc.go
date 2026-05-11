// Package observability is the shared core for nexus's per-aspect
// observability surface — the data layer that both the dashboard
// SPA and the nexus-watch terminal binary render.
//
// The package is pure logic plus types: a Grouper consumes raw
// bridle events plus turn-boundary and chat-side calls from the
// funnel/broker and emits pre-grouped Frames (TurnFrame,
// ChatFrame, PresenceFrame). A Buffer retains recent frames per
// aspect so newcomers can replay tail-on-subscribe. An Artifact
// extractor parses Edit/Write/MultiEdit/NotebookEdit tool inputs
// into a renderer-friendly shape.
//
// No transport, no broker hookup, no UI live here — Phase B will
// wire the Grouper into the broker and define the WS frame
// surface. See docs/2026-05-12-nexus-watch-and-observability-core.md.
package observability
