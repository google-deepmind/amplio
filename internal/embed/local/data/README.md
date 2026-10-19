# Built-in embedding table

`make embed-table` installs `table.bin` here: the static embedding table
(potion-base-32M, PCA to 256 dimensions, int8) that `internal/embed/local`
compiles into the binary with `go:embed`. The generator,
`internal/embed/local/gentable`, downloads the pinned upstream model, verifies
its checksums, derives the table and installs it.

The table is derived at build time and git-ignored. This README is the only
tracked file: it keeps the `go:embed` pattern valid when no table is installed,
in which case the binary ranks recall with BM25 only. `make build LOCAL_EMBED=0`
removes an installed table.
