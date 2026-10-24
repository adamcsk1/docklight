FROM --platform=$BUILDPLATFORM golang:1.27-bookworm AS build
ARG TARGETOS
ARG TARGETARCH
ARG TARGETVARIANT
ARG BUILD_COMMIT=unknown
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} GOARM=${TARGETVARIANT#v} \
    go build -trimpath -ldflags "-s -w -X docklight/internal/buildinfo.Commit=${BUILD_COMMIT}" -o /out/docklight ./cmd/docklight && \
    CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} GOARM=${TARGETVARIANT#v} \
    go build -trimpath -ldflags "-s -w -X docklight/internal/buildinfo.Commit=${BUILD_COMMIT}" -o /out/docklight-agent ./cmd/docklight-agent

FROM build AS test
RUN go test -race -cover ./... && go vet ./...

# Official CLI image includes Compose on amd64, arm64 and arm/v7.
FROM docker:29-cli AS agent
ARG BUILD_COMMIT=unknown
COPY --from=build /out/docklight-agent /usr/local/bin/docklight-agent
LABEL io.docklight.role=agent
LABEL org.opencontainers.image.revision=${BUILD_COMMIT}
EXPOSE 8081
VOLUME ["/data"]
HEALTHCHECK --interval=30s --timeout=5s --start-period=20s --retries=3 \
    CMD addr=${ADDR:-:8081}; host=${addr%:*}; port=${addr##*:}; host=${host#\[}; host=${host%\]}; \
        case "$host" in ""|"*"|"0.0.0.0"|"::") host=localhost;; esac; \
        case "$host" in *:*) host="[$host]";; esac; \
        if [ -n "$TLS_CERT" ]; then docklight-agent -healthcheck; else \
            wget -q -O /dev/null "http://$host:$port/healthz"; fi && docker info >/dev/null 2>&1
ENTRYPOINT ["docklight-agent"]

FROM alpine:3.23 AS ui
ARG BUILD_COMMIT=unknown
RUN apk add --no-cache ca-certificates
COPY --from=build /out/docklight /usr/local/bin/docklight
LABEL io.docklight.role=ui
LABEL org.opencontainers.image.revision=${BUILD_COMMIT}
EXPOSE 8080
VOLUME ["/data"]
HEALTHCHECK --interval=30s --timeout=5s --start-period=20s --retries=3 \
    CMD addr=$(printf '%s\n' "${ADDR:-:8080}" | xargs); addr=${addr:-:8080}; host=${addr%:*}; port=${addr##*:}; \
        host=${host#\[}; host=${host%\]}; \
        case "$host" in ""|"*"|"0.0.0.0"|"::") host=localhost;; esac; \
        case "$host" in *:*) host="[$host]";; esac; \
        base=$(printf '%s\n' "${BASE_URL:-/}" | xargs); \
        while [ "${base#/}" != "$base" ]; do base=${base#?}; done; \
        while [ "${base%/}" != "$base" ]; do base=${base%?}; done; \
        base="/$base"; wget -q -O /dev/null "http://$host:$port${base%/}/healthz"
ENTRYPOINT ["docklight"]
