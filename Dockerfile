# syntax=docker/dockerfile:1
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /out/github-link-matrix-bot .

FROM alpine:3.22
RUN apk add --no-cache ca-certificates tzdata
COPY --from=build /out/github-link-matrix-bot /usr/local/bin/github-link-matrix-bot
USER 65532:65532
VOLUME /data
WORKDIR /data
ENTRYPOINT ["/usr/local/bin/github-link-matrix-bot"]
CMD ["-config", "/data/config.yaml"]
