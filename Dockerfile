FROM golang:1.26.4-bookworm AS build
ARG BUILD_DATE=unknown

WORKDIR /src
COPY go.mod go.sum* ./
RUN go mod download
COPY . .
RUN build_date="${BUILD_DATE}"; \
  if [ "$build_date" = "unknown" ]; then build_date="$(date -u +%Y-%m-%dT%H:%M:%SZ)"; fi; \
  CGO_ENABLED=0 GOOS=linux go build -ldflags "-X main.buildDate=${build_date}" -o /out/docklight ./cmd/docklight

FROM debian:bookworm-slim

RUN apt-get update \
  && apt-get install -y --no-install-recommends ca-certificates curl gnupg \
  && install -m 0755 -d /etc/apt/keyrings \
  && curl -fsSL https://download.docker.com/linux/debian/gpg | gpg --dearmor -o /etc/apt/keyrings/docker.gpg \
  && chmod a+r /etc/apt/keyrings/docker.gpg \
  && echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.gpg] https://download.docker.com/linux/debian bookworm stable" > /etc/apt/sources.list.d/docker.list \
  && apt-get update \
  && apt-get install -y --no-install-recommends docker-ce-cli docker-compose-plugin \
  && rm -rf /var/lib/apt/lists/*

COPY --from=build /out/docklight /usr/local/bin/docklight

EXPOSE 8080
VOLUME ["/data", "/stacks"]

ENTRYPOINT ["docklight"]
