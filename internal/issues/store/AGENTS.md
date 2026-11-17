# internal/issues/store: the native issue store brick (read before working here)

Area: issues. Contract: SPEC "Native issues". Interface:
`store.go` (`Store`, `ProjectStore`, `Feed`, `SQLReader`).

## The seam

The interface every driver implements, and the types it speaks. Drivers: `sqlite` (built); Postgres and Dolt
are planned behind the same interface. The service (`internal/issues`) is the only caller.

## What may cross, and what may not

- Drivers import only `internal/api`, `internal/ids`, `internal/store` (the SQLite file substrate) and this
  package. A driver never imports `internal/issues`, `core`, `authz` or another brick. No test enforces this yet;
  the planned import-boundary test will.
- Drivers know nothing about authz. The service passes the areas a caller may read (`Areas`) and an `Authorize`
  callback that every write calls inside its transaction.
- Every write goes through one funnel, `ProjectStore.Apply(Mutation)`, and records its history rows
  (`api.IssueChange`) in the same transaction. History ids are ULIDs in commit order; `Feed`/`Changes(since)` is
  what replicas and outboxes read. Never write around it.
- Only this brick reads or migrates its tables. Other bricks go through `core.IssueService`.

## Conformance

`storetest.Run(t, newStore)` is the shared suite, through the interface only. Every driver runs it
(`sqlite/conformance_test.go`) and passes before it lands. Generic behaviour goes there; driver tests keep
specifics (files, WAL, migrations, ids). `sqlite/speed_test.go` covers the 16 ms p95 budget on 11.5k issues;
it skips without `ZM_ISSUES_DB` (a copy of a database, never a live one).

Every driver has an interface guard, as `sqlite.go` does (`var _ istore.Store = (*Store)(nil)`). Run
`go test -p 2 ./internal/issues/store/...`.
