# Build from the repo root: podman build -f services/realtime/Dockerfile .
FROM docker.io/library/golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY pkg ./pkg
COPY services/realtime ./services/realtime
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X parley/pkg/buildinfo.Version=${VERSION}" -o /out/realtime ./services/realtime

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/realtime /realtime
EXPOSE 8081
USER nonroot
ENTRYPOINT ["/realtime"]
