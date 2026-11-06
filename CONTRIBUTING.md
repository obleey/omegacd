# Contributing to OmegaCD

Thanks for your interest! OmegaCD is early, so issues with bug reports, rough edges and use cases are as valuable as code.

- **Bugs and ideas:** open an [issue](https://github.com/obleey/omegacd/issues). For larger changes, please open an issue first so we can agree on the approach.
- **Security issues:** do not open a public issue — see [SECURITY.md](SECURITY.md).

## Branches

- **`main` is v0.2 in development**, a breaking redesign to pull-based Agents. The plan, decisions and tickets are in [docs/v0.2](docs/v0.2/README.md); the vocabulary in [GLOSSARY.md](GLOSSARY.md); hard-to-reverse decisions in [docs/adr](docs/adr). Pick a ticket whose dependencies are done and mention its ID in the PR.
- **`release-0.1`** is v0.1 (push over SSH/TLS): bug fixes only; send those PRs to that branch.

## Development

Requires Go (version in `go.mod`), Node.js 24 and a running Docker engine.

```sh
make build              # web UI, then bin/omegacd
make test               # unit tests
make test-integration   # integration tests against the local Docker engine
make e2e                # end-to-end: throwaway OmegaCDs driven through the CLI
cd web && npm run check # type-check the UI
```

Every feature and fix is tested at every level: unit tests for its logic and an end-to-end test in `test/e2e` that drives it through the real binary. Before sending a pull request, the whole suite passes: `go vet ./...`, `make test`, `make test-integration` and `make e2e` (with Forgejo running for the webhook test: `docker compose -f test/e2e/forgejo.yaml up -d`). Keep pull requests focused; one change per PR.

**Never commit secrets** — no age keys, private keys, tokens or databases, not even for tests (generate them at test time). CI runs gitleaks on every push.

## Releasing (maintainers)

Push a tag `vX.Y.Z` on `main` (0.2 and later) or `release-0.1` (0.1.x). The Release workflow builds the binaries (stamped with the tag), the `SHA256SUMS`, the GitHub Release and the multi-arch image `ghcr.io/obleey/omegacd` (`X.Y.Z`, `X.Y`, `latest`; tags with a `-` suffix are pre-releases and do not move `latest`).

After the **first** release, the GHCR package is private: set its visibility to public once under the package's *Package settings → Change visibility*.

By contributing you agree that your contributions are licensed under the [Apache-2.0](LICENSE) license.
