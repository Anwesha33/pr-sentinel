# Multi-stage: the worker needs git (to clone the PR under review) and the Go
# toolchain (to run `go vet` / `go test` against Go repositories), so the
# runtime image is not scratch. That is a deliberate trade: a 15MB image that
# cannot run the code it is reviewing is useless here.
FROM golang:1.25-alpine AS build

WORKDIR /src
RUN apk add --no-cache git

# Dependencies first so a source-only change reuses the module layer.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
ARG TARGET=api
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/app ./cmd/${TARGET}

FROM golang:1.25-alpine AS runtime

RUN apk add --no-cache git ca-certificates tini \
 && adduser -D -u 10001 sentinel

# The agent shells out to git and the language toolchain inside a workspace it
# owns; everything runs as a non-root user with no write access to the image.
WORKDIR /home/sentinel
USER sentinel
ENV WORKSPACE_ROOT=/tmp/pr-sentinel \
    GOCACHE=/tmp/go-build \
    GOMODCACHE=/tmp/go-mod

COPY --from=build /out/app /usr/local/bin/app

ENTRYPOINT ["/sbin/tini", "--", "/usr/local/bin/app"]
