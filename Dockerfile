# syntax=docker/dockerfile:1
FROM golang:1.27-bookworm AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
ARG BINARY
RUN test -n "$BINARY" || (echo "BINARY build-arg is required" && exit 1)
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" \
    -o /out/app ./cmd/${BINARY}

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/app /app
USER nonroot:nonroot
ENTRYPOINT ["/app"]
