# Audiobook Metadata Plugin for Silo

First-party [Silo](https://github.com/Silo-Server/silo-server) metadata provider
for audiobook libraries. It implements `metadata_provider.v1` with capability ID
`audiobook-metadata` and default priority `audiobook = 2`.

## Sources

By default the provider searches Audnexus, Apple Books (iTunes), and
AudiobookCovers. Audible, Storytel, BookBeat, Audioteka, and AudiMeta can be
switched on in the plugin settings; the four scrapers parse public catalog pages
and are limited to six requests per minute each, so a title search that includes
them takes several seconds longer. AudiMeta's hosted API shut down in March 2026.

Every source still resolves an ID it already knows (an ASIN or Apple Books ID
on the item), whether or not it is enabled for title searches.

## Configuration

One global setting, **Search sources**, with a switch per source. Install the
plugin, add **Audiobook Metadata** to an audiobook library's metadata provider
chain, and adjust the sources if the defaults miss your catalog.

## Development

```sh
GOWORK=off go test ./...
GOWORK=off make build
```

## Contributing

Read [CONTRIBUTING.md](CONTRIBUTING.md) before opening a pull request. New
sources and matching changes should start as an issue.

## License

`silo-plugin-metadata-audiobook` is licensed under `AGPL-3.0-only`. See
[LICENSE](LICENSE).
