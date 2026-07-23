# Migration fixtures

These databases are immutable lineage fixtures for the explicit Task 3
cutover tests. They contain no provider credentials, repository contents, or
runtime data. `active-writer.db` is intentionally not a file fixture because
its open transaction is created by the test that owns the writer connection.
