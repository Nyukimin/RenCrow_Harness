// Package rencrowharness embeds the design assets that the runtime needs at the
// byte level: the JSON Schemas, the initial SQL migration and the model prompts.
//
// go:embed cannot reach outside a package directory, and schemas/, migrations/ and
// prompts/ are the design source of truth that must not be copied, so this
// module-root package is the single place they are embedded. Nothing here is
// interpreted; consumers (internal/schemacheck, internal/state/sqlite,
// internal/contextplan) load the files by name.
package rencrowharness

import "embed"

// Schemas holds schemas/*.json.
//
//go:embed schemas/*.json
var Schemas embed.FS

// Migrations holds migrations/*.sql.
//
//go:embed migrations/*.sql
var Migrations embed.FS

// Prompts holds prompts/*.md.
//
//go:embed prompts/*.md
var Prompts embed.FS
