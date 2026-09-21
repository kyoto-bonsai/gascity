# Nudge queue reader/writer split (ga-v07yxe)

The queue state file is atomically replaced, but transactional reads still need
an `flock` so their scan has a defined point relative to writers. The original
ten `withNudgeQueueState` calls all took `LOCK_EX`, even when a poll tick found
no work. The `ga-cssm95` no-op write suppression saved disk writes but left the
lock exclusive.

## Call-site classification

| Call site | Lock path | Reason |
| --- | --- | --- |
| `claimDueQueuedNudgesMatching` | `LOCK_SH` scan, upgrade on maintenance or a due match | No-match scan is read-only. A match moves Pending to InFlight. |
| `listQueuedNudges` | `LOCK_SH` scan, upgrade on maintenance | Filtering buckets is read-only. |
| `listQueuedNudgesForTargetWithClock` | `LOCK_SH` scan, upgrade on maintenance | Filtering buckets is read-only. |
| `rollbackQueuedNudge` | `LOCK_EX` | Moves a nudge to Dead and terminalizes its bead. |
| `enqueueQueuedNudgeWithStoreAndClock` | `LOCK_EX` | Adds or supersedes a nudge. |
| `ackQueuedNudgesWithOutcome` | `LOCK_EX` | Removes and terminalizes a nudge. |
| `releaseQueuedNudgeClaims` | `LOCK_EX` | Moves InFlight back to Pending. |
| `recordQueuedNudgeFailureDetailed` | `LOCK_EX` | Changes attempts, retry timing, or dead status. |
| `recordNudgeDispatchSkips` | `LOCK_EX` | Increments durable counters. |
| `runNudgeQueueMaintenanceSweep` | `LOCK_EX` | Recovers leases, expires entries, repairs backing beads. |

The shared scan treats expired Pending items, expired or lease-ended InFlight
items, and any bead-backed Dead item as maintenance candidates. The last case
is deliberately conservative: dead-letter maintenance can repair a backing
bead without changing `state.json`. A scan that finds no candidate performs no
bead-store I/O and no mutation. The existing lock-free liveness snapshot stays
separate; it does not provide transactional maintenance semantics.

## Upgrade and priority protocol

`ReadThenWrite` holds `LOCK_SH` while loading and checking the state. On a
candidate it releases that lock, acquires `LOCK_EX`, **reloads** `state.json`,
and executes the original maintenance or claim callback. It never mutates the
shared-lock snapshot or assumes the candidate still exists after the gap.
This gives the scan a valid linearization point and avoids lost updates.

A writer publishes a flock-protected marker in `writer-intents/` before trying
the lock. Direct producer writes publish `.producer-*.active`; maintenance
upgrades publish `.maintenance-*.active`. Readers back off while either kind is
queued. Maintenance writers back off while a producer is queued, checking both
before and after gate admission. This gives a producer priority even when it
joins an already busy set of mutating pollers. An abandoned marker has no
flock holder and is removed by the next scan. A stable `state.lock.gate` is
the turnstile. A reader briefly takes its shared lock while acquiring shared
`state.lock`, then releases the gate. A writer holds the gate exclusively from
before acquiring exclusive `state.lock` through commit. Both waits use the
original bounded deadline; the gate path does not change the 8-second writer
failure contract. Old binaries ignore the gate but still use exclusive
`state.lock`, so mixed-version state updates remain serialized during rollout.

The busy-load regression parks 50 maintenance upgrades after their shared
scan, then publishes a producer intent while the gate is held. It pauses the
producer after a failed gate attempt and admits one already queued maintenance
writer. That writer must yield without a state write; after the producer
resumes, its commit is the first physical write. The assertion uses write
order and count, with no latency threshold.

## Poller files

The existing `reapStaleNudgePollers` removes dead `.pid` entries on poller
spawn. It deliberately retains `.pid.lock` inodes because deleting a lock file
while another process has it open can create two independent mutexes for one
key. `gc doctor` now reports total poller-directory entries and the PID/lock
split, with an advisory warning at 1,024 entries. A future lock-inode cleanup
requires a coordinated lock migration; this change does not remove live or
possibly-open lock inodes.
