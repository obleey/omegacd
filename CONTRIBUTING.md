# Contributing to OmegaCD

Thanks for your interest! OmegaCD is early, so issues with bug reports, rough edges and use cases are as valuable as code.

- **Bugs and ideas:** open an [issue](https://github.com/obleey/omegacd/issues). For larger changes, please open an issue first so we can agree on the approach.
- **Security issues:** do not open a public issue — see [SECURITY.md](SECURITY.md).

## Development

Requires Go (version in `go.mod`), Node.js 24 and a running Docker engine.

```sh
make build              # web UI, then bin/omegacd
make test               # unit tests
make test-integration   # integration tests against the local Docker engine
cd web && npm run check # type-check the UI
```

Before sending a pull request: `go vet ./...`, `make test` and `make test-integration` pass, and new behaviour has a test. Keep pull requests focused; one change per PR.

**Never commit secrets** — no age keys, private keys, tokens or databases, not even for tests (generate them at test time). CI runs gitleaks on every push.

## Releasing (maintainers)

Push a tag `vX.Y.Z` on `main`. The Release workflow builds the binaries (stamped with the tag), the `SHA256SUMS`, the GitHub Release and the multi-arch image `ghcr.io/obleey/omegacd` (`X.Y.Z`, `X.Y`, `latest`; tags with a `-` suffix are pre-releases and do not move `latest`).

After the **first** release, the GHCR package is private: set its visibility to public once under the package's *Package settings → Change visibility*.

By contributing you agree that your contributions are licensed under the [Apache-2.0](LICENSE) license.
