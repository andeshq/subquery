# syntax=docker/dockerfile:1
FROM golang:1.27-alpine AS build
# Stamped into the binary by the release workflow; "dev" for a local build.
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /out/subquery ./cmd/subquery

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/subquery /usr/local/bin/subquery
EXPOSE 3000
ENTRYPOINT ["/usr/local/bin/subquery"]
