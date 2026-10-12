# Build from the repo root: podman build -f services/api/Dockerfile .
FROM docker.io/library/golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY pkg ./pkg
COPY services/api ./services/api
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/api ./services/api

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/api /api
EXPOSE 8080
USER nonroot
ENTRYPOINT ["/api"]
