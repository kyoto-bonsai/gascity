---
name: gc-work
description: Finding, creating, claiming, and closing work items (beads)
---

# Work Items (Beads)

Everything in Gas City is a bead — tasks, messages, molecules, convoys.
The `gc bd` CLI is the primary interface for bead CRUD.

## Rig-scoped beads

Each rig has its own `.beads/` database with its own ID prefix (e.g.
`fe-` for frontend, `be-` for beads). **A bead must live in the
same database as the agent that will work on it.** When you sling a bead
to a rig-scoped agent, sling operates on the agent's rig database — so
the bead must already exist there. The bead ID prefix tells you which
rig it belongs to.

Use `gc rig list` to see rig names, paths, and prefixes.

## Creating work

**Use `--rig` to create beads in the right database.** If the work will
be dispatched to a rig-scoped agent, create the bead in that agent's rig:

```
gc bd create "title" --rig frontend         # Create in frontend's db (fe- prefix)
gc bd create "title" --rig beads            # Create in beads db (be- prefix)
gc bd create "title"                        # Create in current directory's .beads/
gc bd create "title" -t bug                 # Create with type
gc bd create "title" --label priority=high  # Create with labels
```

## Finding work

```
gc bd list                                # List beads in current .beads/
gc bd list --rig <rigname>                # List beads in a specific rig
gc bd ready                               # List beads available for claiming
gc bd ready --label role:worker           # Filter by label
gc bd show <id>                           # Show bead details
```

## Claiming and updating

```
gc bd update <id> --claim                 # Claim a bead (sets assignee + in_progress) — races in a multi-agent city; prefer `gc hook --claim --id <id>` there
gc bd update <id> --status in_progress    # Update status
gc bd update <id> --add-label <key>=<value>  # Add/update labels
gc bd update <id> --append-notes "progress..."  # Append a note (does not replace existing notes)
```

**Claim before acting on a bead you found by search, not by pool dispatch.**
If you discovered a `gc.routed_to`-addressed bead by reading mail, the
handoff-watchdog, or `gc bd list` — rather than via your own pool-worker
startup hook — run `gc hook --claim --id <id>` *before* acting on it, not
after. When multiple live sessions of the same persona family are up (the
common case in this fleet), `gc.routed_to` names the whole family, not one
seat, so more than one of you can land on the identical bead with no
arbitration unless you claim first. A refused claim names the live sibling
who already holds it — that is your cue to stand down, not to proceed anyway.

## Closing work

```
gc bd close <id>                          # Close a completed bead
gc bd close <id> --reason "done"          # Close with reason
```

## Hooks

```
gc hook [agent]                        # Show routed work for an agent (defaults to $GC_AGENT)
gc hook --claim                        # Atomically claim one routed work item onto this agent's hook
gc hook --claim --id <bead-id>         # Atomically claim THIS SPECIFIC bead instead of auto-selecting one
```
