// Package web is the observer dashboard.
//
// ADR-009: a thin read-only client of the public API, embedded in the binary so
// a deployment stays a single artifact. It uses exactly the endpoints agents
// use (invariant #5) — if the dashboard could see something agents cannot, the
// API would have a backdoor into state; if it could see less, it would need its
// own queries and would drift.
//
// No build step, no npm, no CDN. A toolchain here would be more machinery than
// the thing it builds, and the CSP-safe single file is what makes it deployable
// anywhere the binary goes.
package web

import "embed"

//go:embed index.html
var FS embed.FS
