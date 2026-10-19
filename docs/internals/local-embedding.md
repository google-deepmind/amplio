# The built-in embedding table

`embed_model = "local"` ranks recall with a static embedding table compiled
into the binary (`internal/embed/local`). This note covers how that table gets
there; the user-facing side is in [models](../guide/models.md#the-local-embedder).

## Pipeline

The table is derived at build time and never committed:

1. `make embed-table` (a dependency of `make build`) runs
   `go run ./internal/embed/local/gentable`.
2. The generator downloads the pinned `tokenizer.json` and `model.safetensors`
   (repo, revision and sha256 are constants in `gentable/main.go`) into
   `${XDG_CACHE_HOME:-~/.cache}/amplio/localembed/<repo>@<revision>/`, retrying
   transient failures, and refuses any file whose size or checksum differs.
3. It derives the table: PCA to 256 dimensions (float64 covariance, axes sign-fixed
   so the output does not depend on the eigensolver), projection of the raw rows,
   then int8 with one scale per row. The result is cached next to the downloads,
   keyed by revision, width, quantization and `generatorVersion`.
4. It installs `internal/embed/local/data/table.bin`, where `go:embed` picks it
   up. Rerunning is a no-op while the installed table matches.

Any failure exits non-zero with the URL, the cache directory and the opt-out,
`make build LOCAL_EMBED=0`, which also deletes an installed table.
`AMPLIO_EMBED_BASE_URL` points the download at a mirror with the same
`<repo>/resolve/<revision>/<file>` layout.

The table file carries the tokenizer vocabulary and settings, so the encoder
can never pair a table with the wrong tokenizer. The embedder's `ModelID`
includes a hash of the table, so cached vectors are invalidated whenever it
changes.

## Without the table

`data/README.md` is the only tracked file in `data/`; it keeps the `go:embed`
pattern valid, so `go build ./...` compiles without the table. `local.Available`
then reports false and the recall indexes run with a nil embedder, ranking by
BM25 alone. Copybara excludes `data/*.bin` (it reads the working tree, ignored
files included), so the mirror never carries the derived weights.

## Changing the model

The recipe (model, width, quantization, BM25 fusion weight) was chosen by
measuring agreement with API-model rankings on real recall queries. Changing
any of it means re-measuring, then: update the pins, bump `generatorVersion`,
regenerate `testdata/parity.json` from the reference implementation, and
update the notice in `scripts/gen-third-party-notices.sh`.
