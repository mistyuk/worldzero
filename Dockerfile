# Built in CI, never on the deployment box (docs/DEPLOY.md).
FROM golang:1.26-alpine AS build

WORKDIR /src

# Dependencies first: they change far less often than the world does.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux go build \
    -trimpath \
    -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/worldd ./cmd/worldd

# Distroless: no shell, no package manager, nothing for an escaped process to
# use. Migrations are embedded in the binary, so nothing else needs to ship.
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/worldd /worldd

USER nonroot:nonroot
EXPOSE 8080

ENTRYPOINT ["/worldd"]
