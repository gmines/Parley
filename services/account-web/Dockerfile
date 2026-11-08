# Build from the repo root: podman build -f services/account-web/Dockerfile .
FROM docker.io/library/golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
COPY services/account-web ./services/account-web
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/account-web ./services/account-web

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/account-web /account-web
EXPOSE 8080
USER nonroot
ENTRYPOINT ["/account-web"]
