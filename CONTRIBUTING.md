# Contributing to Pocket RelayMiner

Thank you for your interest in contributing to Pocket RelayMiner! This document provides guidelines and standards for contributing to the project.

## Table of Contents

- [Development Workflow](#development-workflow)
- [Commit Message Format](#commit-message-format)
- [Code Standards](#code-standards)
- [Testing Requirements](#testing-requirements)
- [Pull Request Process](#pull-request-process)
- [Getting Help](#getting-help)

## Development Workflow

We follow a **dev → main → release** workflow:

### Branch Strategy

```
dev (development) → main (release candidate) → vX.Y.Z (production)
```

**Development Branch (`dev`):**
- Active development happens here
- Push frequently for continuous integration
- Docker image tag: `dev`
- Use for: Feature development, bug fixes, experiments

**Main Branch (`main`):**
- Stable, tested code ready for production
- Merge via Pull Request from `dev`
- Docker image tags: `<commit>`, `rc`
- Use for: Pre-production testing, staging

**Release Tags (`v1.0.0`):**
- Production releases
- Created from `main` branch
- Docker image tags: `1.0.0`, `latest`
- Use for: Production deployments

### Making Changes

1. **Start from dev branch:**
   ```bash
   git checkout dev
   git pull origin dev
   git checkout -b feature/your-feature-name
   ```

2. **Install pre-commit hooks (first time only):**
   ```bash
   make install-hooks
   ```

   This installs a git pre-commit hook that automatically runs `make fmt` and `make lint` before each commit. The hook will:
   - Format your code automatically
   - Catch linting errors before they reach CI
   - Prevent commits with code quality issues

   **Note**: The hook will reject commits that fail linting. Fix the issues before committing.

3. **Make your changes** following our [Code Standards](#code-standards)

4. **Test thoroughly:**
   ```bash
   make fmt      # Format code (also runs automatically via pre-commit hook)
   make lint     # Run linters (also runs automatically via pre-commit hook)
   make test     # Run tests
   make build    # Verify build
   ```

   **Note**: If you installed pre-commit hooks (recommended), `make fmt` and `make lint` run automatically on each commit.

5. **Commit with conventional format** (see below)

6. **Push and create PR to `dev`:**
   ```bash
   git push origin feature/your-feature-name
   # Create PR targeting 'dev' branch
   ```

7. **After merge to dev**, changes will be promoted to main and eventually released

## Commit Message Format

We use [Conventional Commits](https://www.conventionalcommits.org/) for automated changelog generation and release notes.

### Format

```
<type>(<scope>): <subject>

<body>

<footer>
```

### Types

- **feat**: New feature
- **fix**: Bug fix
- **perf**: Performance improvement
- **docs**: Documentation changes
- **build**: Build system or dependency changes
- **ci**: CI/CD configuration changes
- **chore**: Maintenance tasks
- **refactor**: Code restructuring without behavior change
- **test**: Adding or updating tests

### Examples

**Feature:**
```
feat(cache): add warmup for faster cold starts

Implement pond-based worker pool for parallel cache warming.
Eliminates L3 chain queries on first relay, reducing latency
from ~100ms to <1ms.

- Created pond worker pool (configurable concurrency)
- Added Stop() method for cleanup
- Wired into relayer startup

Closes #123
```

**Bug Fix:**
```
fix(balance): correct stake warning field mismatch

Fixed critical config issue where YAML used stake_warning_ratio
but code expected stake_warning_proof_threshold.

- Updated example configs with correct field names
- Added validation in DefaultConfig()
- Updated schema to match code

Fixes #456
```

**Performance:**
```
perf(ci): use native ARM64 runners for 8-10x faster builds

Replaced QEMU-emulated ARM builds with native GitHub ARM runners.

- Split docker-build into separate AMD64/ARM64 jobs
- ARM build time: 40min → 3-5min
- Parallel execution for faster CI feedback
```

### Scope Guidelines

Common scopes:
- `cache`: Caching system (L1/L2/L3)
- `miner`: Miner component (SMST, claims, proofs)
- `relayer`: Relayer component (relay processing, validation)
- `config`: Configuration system
- `ci`: CI/CD workflows
- `docs`: Documentation
- `leader`: Leader election
- `redis`: Redis-related changes

## Code Standards

### Mandatory Requirements

1. **Error Handling**
   - Always check errors
   - Use `fmt.Errorf("context: %w", err)` for wrapping
   - Log errors with context using structured logging
   - Never use `panic()` in production code paths

2. **Logging**
   - Use structured logging: `logger.Info().Str("key", value).Msg("message")`
   - Include relevant context fields for debugging
   - Use appropriate levels: Debug, Info, Warn, Error
   - Never log sensitive data (private keys, credentials)

3. **Concurrency**
   - Use `xsync.MapOf` for lock-free concurrent maps
   - Protect shared state with `sync.RWMutex` when necessary
   - Use `context.Context` for cancellation and timeouts
   - ALWAYS defer `Close()` or cleanup functions

4. **Code Quality**
   ```bash
   make fmt     # Must pass (gofmt -s)
   make lint    # Must pass (golangci-lint)
   make test    # All tests must pass
   ```

   **Enforce automatically**: Run `make install-hooks` to install a pre-commit hook that runs `fmt` and `lint` before each commit.

5. **Performance**
   - Profile before optimizing: `go test -bench . -benchmem`
   - Use Redis pipelining for batch operations
   - Pre-allocate slices when size is known
   - Avoid allocations in hot paths

### Code Style

**Good Example:**
```go
func ProcessRelay(ctx context.Context, relay *Relay) error {
    logger := logging.ForComponent(logger, "relay_processor")

    if err := relay.Validate(); err != nil {
        logger.Warn().
            Err(err).
            Str("session_id", relay.SessionID).
            Msg("relay validation failed")
        return fmt.Errorf("validation failed: %w", err)
    }

    result, err := processWithTimeout(ctx, relay)
    if err != nil {
        return fmt.Errorf("processing failed: %w", err)
    }

    logger.Debug().
        Str("session_id", relay.SessionID).
        Int64("compute_units", result.ComputeUnits).
        Msg("relay processed successfully")

    return nil
}
```

**Bad Example:**
```go
func ProcessRelay(relay *Relay) {
    relay.Validate()  // Not checking error
    process(relay)    // No error handling, no logging
}
```

## Testing Requirements

### Before Submitting PR

All of the following must pass:

```bash
# 1. Format check
make fmt

# 2. Linters
make lint

# 3. Unit tests
make test

# 4. Test coverage
make test-coverage

# 5. Build verification
make build
```

### Writing Tests

- Unit tests for all business logic
- Benchmarks for critical paths (SMST ops, validation, signing)
- Integration tests with miniredis for Redis operations
- Use `-tags test` build constraint for test-only code

### Performance Benchmarks

For performance-critical code:

```go
func BenchmarkCriticalFunction(b *testing.B) {
    // Setup
    for i := 0; i < b.N; i++ {
        // Code to benchmark
    }
}
```

Run benchmarks:
```bash
go test -bench=BenchmarkCriticalFunction -benchmem ./package
```

## Pull Request Process

### Before Creating PR

1. ✅ Rebase on latest `dev` branch
2. ✅ All tests pass (`make test`)
3. ✅ Code is formatted (`make fmt` - automatic if you installed pre-commit hooks)
4. ✅ Linters pass (`make lint` - automatic if you installed pre-commit hooks)
5. ✅ Commit messages follow conventional format
6. ✅ Added/updated tests for new functionality

**Tip**: Install pre-commit hooks (`make install-hooks`) to automatically enforce formatting and linting on every commit.

### PR Requirements

1. **Title**: Use conventional commit format
   - Good: `feat(cache): add warmup for faster cold starts`
   - Bad: `Added cache warmup`

2. **Description**: Include:
   - What changed and why
   - How to test the changes
   - Any breaking changes
   - Related issues (Closes #123)

3. **Testing**: Describe how you tested:
   - Unit tests added/updated
   - Manual testing performed
   - Performance impact (if applicable)

4. **Reviews**:
   - At least 1 approval required
   - Address all review comments
   - Keep PR focused and reasonably sized

### PR Template Example

```markdown
## What Changed

Brief description of the change and motivation.

## How to Test

1. Steps to test the change
2. Expected behavior
3. Screenshots/logs if applicable

## Checklist

- [ ] Tests added/updated
- [ ] Documentation updated
- [ ] Commits follow conventional format
- [ ] All CI checks pass

## Related Issues

Closes #123
```

## Getting Help

### Documentation

- **README.md**: Project overview and quick start
- **CLAUDE.md**: Development guidelines (if using Claude Code)
- **Architecture docs**: See `docs/` directory (if available)

### Communication

- **Issues**: Report bugs or request features
- **Discussions**: Ask questions or propose ideas
- **Pull Requests**: Code contributions

### Development Tools

**Required:**
- Go 1.24.3+
- Docker + Docker Buildx
- Make

**Recommended:**
- Tilt (for local Kubernetes development)
- kubectl (for debugging)
- Redis CLI (for debugging)

**IDE Setup:**
- VSCode: Install Go extension
- GoLand: Built-in Go support
- Vim/Neovim: Use vim-go or coc-go

### Debugging

Use the built-in redis-debug tool:

```bash
# Check leader election
./bin/pocket-relay-miner redis-debug leader

# Inspect sessions
./bin/pocket-relay-miner redis-debug sessions --supplier <address>

# View SMST tree
./bin/pocket-relay-miner redis-debug smst --session <session_id>

# Monitor Redis Streams
./bin/pocket-relay-miner redis-debug streams --supplier <address>
```

## Code of Conduct

- Be respectful and inclusive
- Provide constructive feedback
- Focus on the code, not the person
- Help others learn and grow

## License

By contributing, you agree that your contributions will be licensed under the same license as the project (see LICENSE file).

---

**Thank you for contributing to Pocket RelayMiner!** 🚀
