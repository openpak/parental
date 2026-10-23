// Package migrations embeds the SQL migration files, applied in name order at
// start-up (store.Migrate).
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS
