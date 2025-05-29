# Agent Contribution Guidelines

This repository is open source and used by many users. Robots or autonomous agents contributing here must follow these rules:

1. **Keep the project working**: Do not introduce broken code, misspelled words, or incompatible API changes. Backwards compatibility is required.
2. **Check your changes**: Run `go vet ./...` and `go test ./...` after modifications. If tests fail because dependencies cannot be fetched in your environment, note this in your PR.
3. **Review carefully**: Inspect your changes multiple times and ensure they compile before committing.
4. **Respect existing style**: Keep the existing code and documentation formatting.

