# Build from the repo root: podman build -f services/voice-control/Dockerfile .
FROM docker.io/library/golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY pkg ./pkg
COPY services/voice-control ./services/voice-control
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X parley/pkg/buildinfo.Version=${VERSION}" -o /out/voice-control ./services/voice-control

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/voice-control /voice-control
EXPOSE 8082
USER nonroot
ENTRYPOINT ["/voice-control"]
