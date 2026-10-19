# Two stages: build the static binary, then copy it into a distroless image
# that holds nothing else. No shell, no package manager, no libc.

# BUILDPLATFORM pins the build stage to the native architecture and lets Go
# cross-compile to TARGETARCH. Without it, buildx runs the whole compile under
# QEMU emulation for the foreign arch, which turns a one-minute build into ten.
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build

WORKDIR /src

# Dependencies resolve from go.mod and go.sum alone, so they cache across
# every change that does not touch them.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# CGO_ENABLED=0 is what modernc.org/sqlite buys: pure Go, no libc to link
# against, so the binary runs on a base image with nothing in it.
#
# -trimpath drops local filesystem paths from the binary. -s -w drop the symbol
# table and DWARF, worth about 30% of the size for a daemon nobody is going to
# attach a debugger to in production. The version is stamped here because
# `go build` has no other way to know it.
ARG VERSION=dev
ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} \
    go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/rssdeck ./cmd/rssdeck

# An empty /data owned by the runtime user, to be copied into the final stage.
# It has to be made here: distroless has no shell, so the final stage cannot
# mkdir or chown anything.
RUN mkdir -p /data

FROM gcr.io/distroless/static:nonroot

# CA certificates come with the base image, and HTTPS feeds need them.
COPY --from=build /out/rssdeck /rssdeck

# The SQLite file and its WAL live here. Declaring it means `docker run` without
# a -v still survives a restart, rather than silently re-sending every item
# from an empty database.
#
# The directory is copied in already owned by 65532 rather than left to VOLUME,
# which would create it root-owned: a fresh named volume inherits the image's
# ownership of its mount point, so without this the nonroot process cannot open
# its own database. This is what makes `docker run -v rssdeck-data:/data` work
# with no chown.
COPY --from=build --chown=65532:65532 /data /data
VOLUME /data

EXPOSE 8080

# distroless has no shell and no curl, so the binary probes itself. The 40s
# start period is slack for the migrations and the Readeck probe.
HEALTHCHECK --interval=30s --timeout=5s --start-period=40s --retries=3 \
    CMD ["/rssdeck", "healthcheck"]

# 65532 is distroless's nonroot user. A named volume inherits the directory's
# ownership; a bind mount needs chown 65532:65532, which the README covers.
USER nonroot:nonroot

ENTRYPOINT ["/rssdeck"]
