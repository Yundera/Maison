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
COPY --from=backend /maison /maison
EXPOSE 8080
ENTRYPOINT ["/maison"]
