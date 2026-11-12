# syntax=docker/dockerfile:1

# Build stage. cgo is required: pg_query_go compiles the Postgres parser.
FROM golang:1.27-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
# Link statically (libc included) so the runtime image needs no libc.
# netgo/osusergo: pure-Go DNS and user lookup, so no glibc NSS at run time.
RUN CGO_ENABLED=1 go build -trimpath -tags netgo,osusergo \
      -ldflags="-s -w -X main.version=${VERSION} -linkmode external -extldflags -static" \
      -o /out/adminer-bridge ./cmd/adminer-bridge

# Runtime stage: CA certificates and tzdata only, no libc, no shell, uid 65532.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/adminer-bridge /adminer-bridge
# Mount the config here (read-only). Passwords and the token come from
# environment variables, never from the image.
ENV ADMINER_BRIDGE_CONFIG=/etc/adminer-bridge/config.yaml
EXPOSE 8090
# The image has no curl, so the binary checks itself. If you change the
# listen port, override the healthcheck with: health --url http://127.0.0.1:PORT/healthz
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
  CMD ["/adminer-bridge", "health"]
ENTRYPOINT ["/adminer-bridge"]
CMD ["serve", "--http", ":8090"]
