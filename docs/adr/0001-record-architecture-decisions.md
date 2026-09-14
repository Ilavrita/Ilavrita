# 1. Record architecture decisions

Date: 2026-09-14

## Status

Accepted

## Context

Ilavrita makes a few decisions that are expensive to reverse: which runtime it
builds on, how storage is abstracted, what the public contract is. Those
decisions will be questioned by contributors who were not present for them, and
by us, later.

Reasoning that lives only in a pull request thread is effectively lost.

## Decision

Record architecturally significant decisions as numbered files in `docs/adr`.
Each states context, decision and consequences. A record is never rewritten once
accepted; a later record supersedes it.

## Consequences

The cost is a short document per significant decision. The benefit is that
"because of X" has a citation, and that revisiting a decision starts from what we
actually knew at the time.
