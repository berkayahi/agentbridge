## Summary

Describe the operator-facing behavior and why the change is generic.

## Risk and trust boundaries

- [ ] No personal identity, address, token, private repository detail, or CLI session data is committed.
- [ ] Filesystem paths, remotes, and delivery refs remain configuration-owned.
- [ ] Delivery remains opt-in and exact-ref constrained.
- [ ] Provider tests use fakes and do not make model turns.

## Verification

- [ ] A failing test preceded behavior changes.
- [ ] `go test -race ./...`
- [ ] `go vet ./...`
- [ ] Shell syntax checks, when applicable
- [ ] Documentation updated

List any additional manual or compatibility checks here.
