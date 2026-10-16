# Development

Requires Go 1.26+.

```sh
go build -o nimroute .        # standard build
go build -tags ner .          # include ONNX NER PII model support
```

## Optional ONNX NER (`-tags ner`)

By default, PII detection uses fast regex/heuristic recognizers. Building with `-tags ner` adds the Rampart ONNX NER model.

Fetch the model weights (not committed to git):

```sh
./pii/fetch_model.sh
go vet -tags ner ./...
```

## Pre-commit checks

```sh
gofmt -l .        # must output nothing
go vet ./...
go build ./...
```

## Testing on a scratch port

Never interrupt a running instance on `:5419` to test a change. Copy `config.yml` to a scratch file, pick an unused port, and clean up afterwards:

```sh
cp config.yml config.test.yml
./nimroute --config config.test.yml --port 5420
rm -f config.test.yml
```

## Codebase rules

- Keep diffs surgical: every changed line should trace directly to the task.
- New config keys go in both `config.yml.example` (with a comment) and `defaultConfig()` in `config.go`.
- `true`-by-default booleans in `defaultConfig()` need no extra presence code: `yaml.v3` leaves omitted keys untouched, so only an explicit `false` flips them.
- New `/status` fields need a `status.show_*` flag and unauthenticated redaction in `StatusFor`.
- New forwarded upstream headers must pass the credential deny-list (`authorization`, `cookie`, `x-api-key`, `x-session-id`, etc.).

See [`AGENTS.md`](../AGENTS.md) for the full list of invariants.
