# Agent contract — read before writing any code

The module is `github.com/dharaneswar339/durexec`. Go 1.25.

`internal/core` is written and frozen. It holds every shared type: identifiers,
events and their payloads, commands, `Decision`, the error taxonomy, and the
ports (`EventStore`, `Committer`, `TaskQueue`, `LeaseStore`, `TimerStore`,
`IntentStore`, `Embedder`, `ExecutionStore`, `Clock`).

## Rules

1. **Do not edit `internal/core`.** If you need something it lacks, define it in
   your own package, or report the gap in your final message. Do not change a
   shared type — other agents are compiling against it right now.
2. **Only create files inside the directories you were assigned.** Another agent
   owns every other directory. Never edit a file you did not create.
3. **Depend on the interfaces in `core`, not on other agents' packages.** If you
   need behaviour another package will provide, take it as an interface
   parameter. This is what lets us all work at once.
4. **Do not add third-party dependencies** except the ones named in your brief.
   Do not run `go mod tidy` — it will fight other agents. Add requires to
   `go.mod` only if your brief says to; otherwise use the standard library.
5. **Do not run `go build ./...` or `go test ./...`** across the whole repo — it
   will fail until every agent lands. Build and test only your own package:
   `go vet ./internal/<yours>/...` and `go test ./internal/<yours>/...`.
6. **Write table-driven unit tests** that need no database, unless your brief
   says otherwise. Tests must pass before you finish.
7. **Comment the why, not the what.** Match the density in `internal/core`.
   Explain a design decision where one was made; skip narrating obvious code.
8. Run `gofmt -w` on everything you write.

## Schema

`migrations/0001_init.up.sql` is written and frozen. Read it for exact column
names and types before writing SQL.

## Definition of done

`gofmt` clean, `go vet` clean on your package, your own tests passing, and a
final message that states what you built, any gap you found in `core`, and
anything the integrator must wire up.
