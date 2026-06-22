# Build a static binary, then ship it on a minimal distroless base.
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY . .
ARG VERSION=docker
ARG COMMIT=none
RUN CGO_ENABLED=0 go build \
    -ldflags "-s -w -X github.com/Leo-Zh9/vantage/internal/version.Version=${VERSION} -X github.com/Leo-Zh9/vantage/internal/version.Commit=${COMMIT}" \
    -o /vantage ./cmd/vantage

FROM gcr.io/distroless/static-debian12
COPY --from=build /vantage /vantage
EXPOSE 8080 9090
ENTRYPOINT ["/vantage"]
