# Agent Instructions

## Development mode

During this implementation phase, prioritize building the planned product over test and verification work.

- Do not add new test cases by default.
- Do not run full test suites, race checks, vet, lint, static analysis, cross-platform builds, or browser verification unless the user explicitly requests them or they are needed to diagnose a concrete failure.
- Do not pause implementation for routine verification. Preserve existing tests and production checks, but defer expanding or running them.
- Keep dependency chains sequential. Parallelize only independent work in separate worktrees, according to the existing implementation plan.
- Do not change the implementation plan unless the user explicitly asks for a plan change.
- Report blockers and material design decisions promptly, then continue with the next implementation batch.

This policy is intentionally temporary for the current build phase. A later release or integration request may re-enable the normal verification gates.
