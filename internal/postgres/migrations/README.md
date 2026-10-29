# PostgreSQL migrations

StillFS embeds numbered SQL files and applies each unapplied version in its own
transaction. Applied files must be treated as immutable; make later changes in
a new version. Keep object payload bytes out of PostgreSQL. See
`docs/architecture/metadata-layer.md` for schema and failure semantics.
