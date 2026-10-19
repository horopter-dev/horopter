# Issue tracker: Linear

Issues for Horopter live in Linear: workspace `horopter`, team **`HOR` (Horopter)**, identifiers
`HOR-N`. GitHub holds code and pull requests only; its closed issues are history, each pointing
to the Linear issue it moved to (`decisions/2026-10-06-issues-move-to-linear.md` in the sibling
`horopter-internal` checkout).

Issues filed on this repository's GitHub arrive in `HOR` through Linear's GitHub sync. Give
them `needs-triage`. Once the repository is public, a synced issue's comments are public too;
keep internal discussion on unsynced HOR issues.

Use the `linear-horopter` MCP server (`mcp__linear-horopter__*`). A Linear server connected to
another workspace answers `HOR-N` as not found. If it needs authentication, ask Robin to run
`/mcp` and authenticate `linear-horopter`.

## Conventions

- **Create an issue**: `save_issue` with `team: "HOR"`, a `title` and a Markdown
  `description`. New issues start in **Backlog** unless asked otherwise.
- **Read an issue**: `get_issue` with the `HOR-N` identifier, plus `list_comments` for its
  discussion. Pass `includeRelations: true` when blocking matters.
- **List issues**: `list_issues` with `team: "HOR"`, filtered by `state`, `label`, `parentId`
  or `project` as needed.
- **Comment**: `save_comment` on the issue.
- **Labels**: `save_issue` with `addLabels` / `removeLabels`. `labels` replaces the whole set,
  so avoid it for incremental changes.
- **Close**: `save_issue` with `state: "Done"` when finished, or `state: "Canceled"` when not
  planned, after a comment saying why.

**Workflow states**: Backlog, Todo, In Progress, In Review, Done, Canceled, Duplicate. The team
has no Triage state; triage is a label (see `docs/agents/triage-labels.md`).

**Area labels**, one per issue, in the `Area` group: Ops, Infra, Docs, B — management API.

## Linking pull requests

A pull request names its issue by identifier, `HOR-N`, in its title or body; GitHub's
closing keywords do not reach Linear. Closing the Linear issue when the PR merges is part of
finishing the work.

## Pull requests as a triage surface

**PRs as a request surface: no.**

## When a skill says "publish to the issue tracker"

Create a Linear issue in team `HOR`.

## When a skill says "fetch the relevant ticket"

`get_issue` with its `HOR-N`, then `list_comments`.

## Parents and blocking

- **Parent and child**: Linear sub-issues. Set `parentId: "HOR-N"` on the child.
- **Blocking**: Linear's native relations. Set `blockedBy: ["HOR-N", …]` on the blocked issue.
  Relations are append-only through `save_issue`; remove one with `removeBlockedBy`.
- **Frontier**: the open children of a parent (`list_issues` with `parentId`) whose blockers
  are all Done or Canceled (`get_issue` with `includeRelations: true`) and that have no
  assignee; first by identifier wins.
- **Claim**: `save_issue` with `assignee: "me"`, the first write.
- **Resolve**: comment with the outcome, then set `state: "Done"`.

## Wayfinding operations

Used by `/wayfinder`. The **map** is one issue with **child** issues as its tickets, using the
parent, blocking, frontier, claim and resolve operations above.

- **Map**: an issue labelled `wayfinder:map`, holding the Destination, Notes,
  Decisions-so-far, Not yet specified and Out of scope sections in its description.
- **Child ticket**: an issue with `parentId` set to the map, labelled with its type:
  `wayfinder:research`, `wayfinder:prototype`, `wayfinder:grilling` or `wayfinder:task`. Add
  the matching `Area` label when one applies.
- **Blocking**: native relations, wired in a second pass once every ticket has an identifier.
- **Frontier query**: `list_issues` with `parentId` set to the map and no `state` filter (it
  takes one state type, and open tickets span several), with `statusType` and `assignee` in
  `fields`. Drop tickets whose status type is `completed` or `canceled`, or that have an
  assignee, then any with a blocker not yet Done or Canceled (`get_issue` with
  `includeRelations: true`); first by identifier wins.
- **Resolve**: comment the answer, set `state: "Done"`, then add a line to the map's
  Decisions-so-far with `save_issue`'s `patch` (`insert_after` the section heading), so a
  concurrent session's edit to another section is not overwritten.
- **Out of scope**: set the ticket to Canceled with a comment saying why, and add a line to the
  map's Out of scope section.
