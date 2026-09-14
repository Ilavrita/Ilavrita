# Governance

Ilavrita is an open-source project developed under Vestcodes product governance.
This document describes who decides what, and how that changes.

## Roles

**Contributor** — anyone who opens an issue, reviews a change or sends a pull
request. No process to join.

**Maintainer** — reviews and merges changes in an area, and is accountable for
its quality. Listed in [MAINTAINERS.md](MAINTAINERS.md).

**Project lead** — arbitrates when maintainers do not reach agreement, and owns
releases, licensing and the public product boundary. Currently
[@0xvestcodes](https://github.com/0xvestcodes).

## Becoming a maintainer

Sustained, high-quality contribution in an area, plus the judgement to say no to
changes that do not belong. Existing maintainers nominate; the project lead
confirms. A maintainer who steps back or becomes inactive moves to emeritus;
nothing about that is a judgement of their work.

## Making decisions

Most decisions are made in the open on issues and pull requests, and most are
uncontroversial. Where maintainers disagree, the aim is consensus; failing that,
the project lead decides and records why.

Decisions that change the architecture are written down as an
[architecture decision record](docs/adr) so the reasoning survives the people who
made it.

## Scope control

Ilavrita has an explicit v0.1 boundary and an explicit list of non-goals, both
set by the product requirements. A change that widens the boundary needs product
approval before it needs code review — this is the main reason a well-written
pull request gets declined.

## What is not decided here

Licensing terms, the contributor agreement, trademark policy and any compliance
claim are owned by Vestcodes and require legal review. Maintainers cannot grant
exceptions to them.

## Changing this document

By pull request, approved by the project lead.
