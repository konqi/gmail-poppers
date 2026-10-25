FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/gmail-poppers ./cmd/gmail-poppers \
    && mkdir -p /out/data

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/gmail-poppers /usr/local/bin/gmail-poppers
# Pre-create /data so a fresh named volume is writable by the nonroot user.
COPY --from=build --chown=65532:65532 /out/data /data
VOLUME /data
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s CMD ["/usr/local/bin/gmail-poppers", "healthcheck"]
ENTRYPOINT ["/usr/local/bin/gmail-poppers"]
