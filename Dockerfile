# Base images: latest stable releases (Go 1.27.2, Node 24 LTS, Alpine 3.24).

# 1. Build the Svelte web UI. On the build machine's own platform: the result
#    is the same for every image.
FROM --platform=$BUILDPLATFORM node:24-alpine3.24 AS web
# The latest npm, not the one bundled with Node (scripts/check.sh keeps it current).
RUN npm install -g npm@12.2.0 --no-audit --no-fund
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN npm ci --no-audit --no-fund
COPY web/ ./
RUN npm run build

# 2. Build the Go binary with the UI embedded, on the build machine's own
#    platform too, cross-compiled for each image's (amd64, arm64).
FROM --platform=$BUILDPLATFORM golang:1.27.2-alpine3.24 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=web /src/web/dist ./web/dist
# Guard: no image from code that fails vet or the tests.
RUN go vet ./... && go test ./...
ARG VERSION=0.23.3
# Where this build's source code is (Homestack is AGPL): a fork sets its own.
ARG SOURCE_URL=https://github.com/leviathonbeast/homestack
ARG TARGETOS TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
    -ldflags "-s -w -X main.version=${VERSION} -X main.source=${SOURCE_URL}" -o /out/homestack .

# 3. Small runtime image. The Docker CLI and Compose plugin are needed to
#    run `docker compose` against the host's Docker (through the socket).
FROM alpine:3.24
LABEL org.opencontainers.image.title="Homestack" \
      org.opencontainers.image.licenses="AGPL-3.0-or-later"
# libvirt-client provides virsh, for the optional VMs page. `apk upgrade`
# picks up security fixes released since the base image was published; build
# with --pull (`make docker` and compose.yaml do) for the latest base images.
RUN apk upgrade --no-cache && \
    apk add --no-cache docker-cli docker-cli-compose libvirt-client ca-certificates tzdata
COPY --from=build /out/homestack /usr/local/bin/homestack
# Its licence, and those of the libraries built into it (also served at
# /third-party-notices.txt).
COPY --chmod=0644 LICENSE web/public/third-party-notices.txt /usr/share/licenses/homestack/
ENV HOMESTACK_DATA=/data \
    HOMESTACK_STACKS=/opt/stacks \
    HOMESTACK_PORT=5080 \
    HOMESTACK_IN_CONTAINER=1
VOLUME /data
EXPOSE 5080
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 CMD ["homestack", "healthcheck"]
ENTRYPOINT ["homestack"]
