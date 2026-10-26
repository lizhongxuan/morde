package migrations

import "embed"

// FS contains the versioned SQL migrations applied by the explicit migrate command.
//
//go:embed *.sql
var FS embed.FS
