# Publish-server image. Unlike the broker, this one keeps the Go toolchain and
# git at RUNTIME: the server compiles each submitted adapter with `go build` and
# triggers the publish workflow with `git`. So it runs on the golang base, not
# a slim runtime image.
#
# Build from the repo root:
#   docker build -f deploy/docker/publish-server.Dockerfile -t pilot-publish-server .
FROM golang:1.25
RUN apt-get update \
 && apt-get install -y --no-install-recommends git ca-certificates \
 && rm -rf /var/lib/apt/lists/*
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
# Build the server, and warm the module cache so runtime adapter builds are fast.
RUN go build -o /usr/local/bin/publish-server ./cmd/publish-server
EXPOSE 8080
# Store + platform key persist on a mounted volume; tokens come from the env.
ENTRYPOINT ["publish-server"]
CMD ["-addr", ":8080", "-store", "/data/store", "-key", "/data/platform.key"]
