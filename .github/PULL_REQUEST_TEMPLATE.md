<!-- Thanks for contributing to tobira! Please fill this out so reviewers have context. -->

## What & why

<!-- What does this change do, and why is it needed? Link any related issue. -->

Closes #

## Type of change

- [ ] Bug fix (non-breaking)
- [ ] New feature (non-breaking)
- [ ] Breaking change
- [ ] New or changed driver
- [ ] Docs
- [ ] CI / release / tooling

## Checklist

- [ ] `make test` passes for every affected module
- [ ] `go vet ./...` and `golangci-lint run` are clean (`make lint`)
- [ ] New/changed behavior is covered by tests
- [ ] Public API changes have doc comments
- [ ] Credentials and connection strings are never logged

## New or changed driver

- [ ] The driver passes `drivertest.Run`
- [ ] Errors are classified
- [ ] Read-only layers are documented
- [ ] README and site page updated

## Notes for reviewers

<!-- Anything else worth calling out. -->
