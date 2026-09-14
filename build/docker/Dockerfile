# syntax=docker/dockerfile:1

# Ilavrita compiles against the pinned PocketBase fork in third_party, so the
# build context must include the initialised submodule.
# Base images are pinned by digest. A tag is mutable, so tag-only pinning
# means the image you audited and the image you shipped can differ.
# Dependabot keeps these current.
FROM golang:1.27-alpine@sha256:cf6fca6641884b8433441b2b0652976f975e1d0fdd26d177eaaf8596087f3125 AS build

ARG VERSION=dev
ARG REVISION=unknown

WORKDIR /src

COPY go.mod go.sum ./
COPY third_party/pocketbase ./third_party/pocketbase
RUN go mod download

COPY apps ./apps
COPY packages ./packages

# SQLite comes from a pure-Go driver, so the binary needs no C toolchain and can
# run on a scratch image.
ENV CGO_ENABLED=0
RUN go build \
      -trimpath \
      -ldflags "-s -w -X main.version=${VERSION} -X main.revision=${REVISION}" \
      -o /out/ilavrita \
      ./apps/ilavrita

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab

LABEL org.opencontainers.image.title="Ilavrita" \
      org.opencontainers.image.description="Open healthcare infrastructure. FHIR-native, self-hostable, edge to enterprise." \
      org.opencontainers.image.url="https://ilavrita.health" \
      org.opencontainers.image.source="https://github.com/Ilavrita/Ilavrita" \
      org.opencontainers.image.licenses="AGPL-3.0-only"

COPY --from=build /out/ilavrita /usr/local/bin/ilavrita
COPY LICENSE NOTICE /usr/local/share/ilavrita/

# Persist the SQLite database and locally stored files outside the container.
VOLUME ["/data"]
EXPOSE 8090
USER nonroot

ENTRYPOINT ["/usr/local/bin/ilavrita"]
CMD ["serve", "--http=0.0.0.0:8090", "--dir=/data"]
