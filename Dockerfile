FROM --platform=$BUILDPLATFORM golang:1.27-bookworm AS build
ARG TARGETOS
ARG TARGETARCH
ARG TARGETVARIANT
ARG BUILD_DATE=unknown
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} GOARM=${TARGETVARIANT#v} \
    go build -trimpath -ldflags "-s -w -X main.buildDate=${BUILD_DATE}" -o /out/docklight ./cmd/docklight && \
    CGO_ENABLED=0 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH:-amd64} GOARM=${TARGETVARIANT#v} \
    go build -trimpath -ldflags "-s -w" -o /out/docklight-agent ./cmd/docklight-agent

FROM build AS test
RUN go test -race -cover ./... && go vet ./...

# Official CLI image includes Compose on amd64, arm64 and arm/v7.
FROM docker:29-cli AS agent
COPY --from=build /out/docklight-agent /usr/local/bin/docklight-agent
LABEL io.docklight.role=agent
EXPOSE 8081
VOLUME ["/data"]
ENTRYPOINT ["docklight-agent"]

FROM alpine:3.23 AS ui
RUN apk add --no-cache ca-certificates
COPY --from=build /out/docklight /usr/local/bin/docklight
LABEL io.docklight.role=ui
EXPOSE 8080
VOLUME ["/data"]
ENTRYPOINT ["docklight"]
