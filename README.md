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

Install the plugin and add **Audiobook Metadata** to an audiobook library's
metadata provider chain. It has three global settings:

- **Search sources**: a switch per source. Adjust them if the defaults miss
  your catalog.
- **Request limits**: how many requests a minute the plugin sends to Audnexus,
  100 by default. The public Audnexus API allows 300 a minute for each
  address. A higher limit matches a large library sooner; the setting accepts
  20 to 300.
- **Audible store**: the Audible store that Audnexus, AudiMeta and the Audible
  scraper look books up in, the United States by default. ASINs are regional,
  so a book bought on another store's site may have no record in the US store.

## Development

```sh
GOWORK=off go test ./...
GOWORK=off make build
```

## Releases

Releases are signed with GitHub artifact attestations. [CONTRIBUTING.md](CONTRIBUTING.md#release)
describes how maintainers cut a release and how to verify a downloaded binary.

## Contributing

Read [CONTRIBUTING.md](CONTRIBUTING.md) before opening a pull request. New
sources and matching changes should start as an issue.

## License

`silo-plugin-metadata-audiobook` is licensed under `AGPL-3.0-only`. See
[LICENSE](LICENSE).
