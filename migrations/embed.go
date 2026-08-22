// Package migrations embeds the SQL migrations into the worldd binary so a
// deployment is a single artifact: no migration files to ship alongside it and
// no chance of running a binary against schema it was not built for.
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS
