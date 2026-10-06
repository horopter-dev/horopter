# Domain Docs

How the engineering skills should consume Horopter's domain documentation when exploring.

## Before exploring, read these

The domain docs live in the sibling `horopter-internal` checkout, beside this repository:

- **`../horopter-internal/CONTEXT.md`**: Horopter's ubiquitous language and context map. Terms
  marked ● exist in code; ○ are designed but unbuilt, so never describe a ○ term in this
  repository as if it worked.
- **`../horopter-internal/decisions/`**: the decision records, the equivalent of ADRs. Named
  `YYYY-MM-DD-<topic>.md`, not numbered. Read those that touch the area you are about to work
  in; `rg -l <term> ../horopter-internal/decisions/` finds them.

Specs and plans are in `../horopter-internal/docs/superpowers/`.

If the checkout isn't there, as for anyone without access to `horopter-internal`, **proceed
silently** without them.

## File structure

Single-context, split across two repositories:

```
horopter-dev/
├── horopter/              ← this repository: code
│   └── docs/agents/
└── horopter-internal/     ← private: domain language and decisions
    ├── CONTEXT.md
    └── decisions/
```

## Use the glossary's vocabulary

When your output names a domain concept (in an issue title, a refactor proposal, a hypothesis,
a test name), use the term as defined in `CONTEXT.md`. Don't drift to synonyms the glossary
explicitly avoids.

If the concept you need isn't in the glossary yet, that's a signal: either you're inventing
language the project doesn't use (reconsider) or there's a real gap (note it for
`/domain-modeling`).

## Flag decision conflicts

If your output contradicts a decision record, surface it explicitly rather than silently
overriding:

> _Contradicts `decisions/2026-09-16-out-of-order-handling-belongs-in-the-projection.md`, but
> worth reopening because…_

New or replacing decisions are written in `horopter-internal`, not here.
