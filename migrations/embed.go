package migrations

import "embed"

// FS holds numbered .sql files applied by store.Migrate.
//
//go:embed *.sql
var FS embed.FS
