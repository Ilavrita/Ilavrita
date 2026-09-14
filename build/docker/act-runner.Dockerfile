# syntax=docker/dockerfile:1

# Runner image for local workflow runs with act.
#
# actions/setup-go rewrites PATH and drops act's own node directory, which then
# breaks every JavaScript action that runs after it. Real GitHub runners keep
# node on a path that survives, so this only bites locally. Linking node where
# setup-go leaves it reachable makes local runs behave like CI.
# Local developer tooling only, so a floating tag is acceptable here; the
# released image in Dockerfile is digest-pinned.
FROM catthehacker/ubuntu:act-latest

RUN ln -sf "$(command -v node)" /usr/local/bin/node \
 && ln -sf "$(command -v npm)" /usr/local/bin/npm \
 && node --version && npm --version
