# Broker image: the managed-key gateway. A self-contained static binary (the
# SQLite driver is pure Go, so CGO is off) on distroless — tiny and no shell.
#
# Build from the repo root:
#   docker build -f deploy/docker/broker.Dockerfile -t pilot-broker .
FROM golang:1.25 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/broker ./cmd/broker

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/broker /broker
EXPOSE 8099
# Registry + durable usage store live on a mounted volume; the master keys come
# from the environment (never baked into the image).
ENTRYPOINT ["/broker"]
CMD ["-registry", "/registry/apps.json", "-addr", ":8099"]
