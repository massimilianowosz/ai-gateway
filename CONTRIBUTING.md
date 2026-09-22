# Contributing to ubiquum-ai-gateway

## Development

```bash
# Clone
git clone https://github.com/ubiquum-ai/ubiquum-ai-gateway.git
cd ubiquum

# Build
make build

# Test
make test

# Lint (requires golangci-lint)
make lint
```

## Pull Requests

1. Fork the repo and create a branch from `main`
2. Add tests for new functionality
3. Ensure `make test` and `make lint` pass
4. Keep commits focused and atomic

## Adding a Provider

1. Create `internal/provider/<name>/<name>.go` implementing the `provider.Provider` interface
2. Register it in the provider factory in `cmd/gateway/main.go`
3. Add config documentation
4. Add at least one unit test

## Code Style

- Follow standard Go conventions (`gofmt`, `goimports`)
- Unexported types/functions unless needed externally
- Error messages: lowercase, no trailing punctuation
- Tests: table-driven where practical

## Reporting Issues

Use GitHub Issues. Include:
- Go version (`go version`)
- Config (redact keys)
- Steps to reproduce
- Expected vs actual behavior
