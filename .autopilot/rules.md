# Autopilot rules: decypharr

## Checks
Per item (from the repo root, every step's exit code checked, stop on the first failure):
`go build ./... && go vet ./... && go test ./... && golangci-lint run ./...`
Before every push, also `nix build .#default` (catches a stale vendorHash and Go version pin).
If the disk fills ("no space left"), `go clean -cache` and rerun; never commit on a partial gate.
New logic is TDD: the regression test fails before the fix and passes after (mutation-check it).
Final item: the full gate on `minz`, plus `git status` clean and `minz` == `origin/minz`.

## May happen mid-run
- Commits on local `minz` (the run branch). Style: `<pkg>: <what>`, one subject line, no Co-Authored-By trailer.
- Ship one item (or one `ship:` group) per post, after its gate, `nix build .#default` and the Opus reviewer pass:
  `git push origin <sha>:refs/heads/ship/decypharr-N`, then post
  `<UTC> decypharr-N → nixfiles: <sha> (ship/decypharr-N, item <n>): what changed, what to verify live`.
  nixfiles deploys exactly that sha. The next ship waits for `ack decypharr-N` (exit `WAIT ack decypharr-N`).
- After `ack decypharr-N`: `git push origin <sha>:refs/heads/minz` (fast-forward only, the acked sha, never local
  `minz` when it is ahead). A `reject`/`nack` never touches origin/minz.
- `ack decypharr-N` = deployed, `just admin health` clean, immediate checks passed. Live behaviour arrives later as
  `watch decypharr-N: <result>`; nothing waits for a watch line.
- Run each push exactly as written above, one per call, no `nix develop -c` wrapper, no pipes or chains.
- Never force-push, delete a remote ref or push any other ref. Never edit ~/projects/nixfiles. No tags, no releases,
  no deploys from here.

## Reviewer stand-in
Before every push: the `reviewer` agent with `model: opus` on the commit range being pushed, plus its ponytail-review pass.
Fix every correctness/safety finding and re-review; record skipped nits in log.md with the reason.
Opus is required (not optional) for anything that deletes, blocklists, fails grabs, removes queue
records or files, or changes safety/fallback code. The reviewer also judges irreversibility: if it says
a change can't be undone by a rollback, STOP.

## Fleet
Subagents via the Agent tool only, Sonnet for implementation/search, Haiku for read-only lookups.
Worktrees by hand from `minz`: `git worktree add -b wip-<x> .claude/worktrees/w-<x> minz`
(isolation: worktree bases on origin/main, not minz). Agents report back in-session; none outlive the session.

## Stop list
Exit STOP for:
- config or state-format changes a rollback can't undo (config.json schema, appendstore/kvstore record
  format, on-disk cache layout, migrations)
- deleting data: library files, cache, store records, arr entries, anything on media-0
- any direct access to media-0 (SSH, API calls, Loki): live reads go through the board as requests to nixfiles
- a nixfiles ack or watch line reporting a regression from our deploy (stop and wait for the owner; nixfiles rolls back)
- weekly utilization above the plan's ceiling
- a design choice the handoff/plan doesn't settle (naming, new options, behaviour the owner hasn't approved)

## Where to look for more work
Open mandate: decypharr correctness (imports, streams, queue/blocklist decisions, reacquire).
- ~/.claude/plans/handoff-2026-10-*-decypharr*.md: open items and "related finding" sections
- ~/.claude/plans/decypharr-handoff.md: watch lists and open items in the latest "State" section
- ~/projects/nixfiles/docs/decypharr-timeouts-roadmap.md: "Unsure" section (read only; don't edit)
- the nixfiles board file: live findings it reports from Loki/arr APIs
- failing or flaky tests, `go test -race` on touched packages
Out: anything the plan's Parking lot names, settings UI and upstream merges beyond the plan's items, lint-only sweeps, refactors with no
behaviour fix. Each new item needs evidence (a log line, a failing test, a live report) and a check.

## Long commands
- `go test ./...` and `nix build .#default` can take several minutes: give the Bash call `timeout: 600000`.
  If a run still times out, launch it detached (`setsid … </dev/null >"$log" 2>&1 & echo $!`) and exit `WAIT 30 pid=<pid>`.

## Shared tool locks
- golangci-lint takes a machine-wide lock; another run may hold it. On its "parallel golangci-lint is running"
  message, wait a minute and retry (up to 10 times) instead of failing the item.

## Peers
- decypharr produces, nixfiles serves it (nixfiles' plan has `serves: decypharr`) on the shared board.
- nixfiles deploys the posted sha (ship/decypharr-N) to minz-media-0, one deploy per ack, and owns the live reads.
- Pacing: at most one decypharr post waiting for an ack; `ahead-cap: 2` lets one more item be committed meanwhile.
- nixfiles' deploys stall ~30 min on the attic copy: an ack within ~1h is normal, `ack-timeout` covers worse.
