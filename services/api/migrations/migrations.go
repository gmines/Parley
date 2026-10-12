// Package migrations embeds the api service's SQL schema migrations.
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS
