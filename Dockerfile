# syntax=docker/dockerfile:1

# 1) Build the Svelte UI -> internal/ui/dist
FROM node:lts-slim AS ui
WORKDIR /src/web
ENV NODE_OPTIONS=--max-old-space-size=4096
COPY web/package.json web/package-lock.json* ./
RUN npm install --no-audit --no-fund
COPY web/ ./
RUN npm run build   # writes to /src/internal/ui/dist

# 2) Build the Go binary with the UI embedded
FROM golang:1.26-alpine AS backend
WORKDIR /src
RUN apk add --no-cache git
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=ui /src/internal/ui/dist ./internal/ui/dist
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /maison ./cmd/maison

# 3) Minimal runtime: the binary + the docker compose plugin (installs shell out
#    to `docker compose`) + bash (for x-casaos pre/post-install hooks).
FROM alpine:3.20
RUN apk add --no-cache ca-certificates docker-cli docker-cli-compose bash tzdata

# HOOK ABI — the complete set of commands available to app lifecycle hooks
# (x-casaos pre/post-install-cmd, x-compose-app hooks). PATH is pointed here by
# internal/stackup/hookshell.go, and anything outside it fails the hook with a
# message naming the sanctioned alternative.
#
# THIS LIST IS A PUBLIC CONTRACT. App authors in three Yundera stores and any
# number of third-party ones write against it. Adding an entry is a compatible
# change; REMOVING ONE BREAKS PUBLISHED APPS. The list below is the set actually
# used by shipped hooks, not a guess.
#
# Deliberately absent, and why the list is an allowlist rather than a set of
# exclusions:
#   - openssl, curl, python, jq, git — not in the image at all. A hook that calls
#     one inside "$(...)" gets an empty string and still exits 0, which is how an
#     app ships an empty secret and installs green. Use `docker run` instead:
#     it fails loudly.
#   - sysctl, ip, mount, adduser, modprobe, chroot, reboot (~31 busybox applets)
#     — present in this image but scoped to THIS CONTAINER, not the host. They
#     appear to work and change nothing. Host access goes through an explicit
#     `docker run --privileged` / `-v /:/host` recipe; see docs/x-compose-app.md.
# An exclusion list would have to be re-audited on every alpine and busybox bump.
# This one fails closed.
RUN mkdir -p /opt/maison/hookbin && \
    for c in cat chmod chown cp cut date dirname echo env expr find grep head \
             id install ln ls md5sum mkdir mktemp mv od printf readlink realpath \
             rm rmdir sed seq sha256sum sleep sort stat tail tee test timeout \
             touch tr uniq wc wget xargs; do \
      ln -s /bin/busybox /opt/maison/hookbin/$c; \
    done && \
    ln -s /usr/bin/docker /opt/maison/hookbin/docker && \
    ln -s /bin/bash /opt/maison/hookbin/bash && \
    ln -s /bin/sh /opt/maison/hookbin/sh

COPY --from=backend /maison /maison
EXPOSE 8080
ENTRYPOINT ["/maison"]
