FROM golang:1.22-bullseye AS build

RUN apt update && update-ca-certificates --fresh && apt -y install build-essential

WORKDIR /go/src/build
COPY ./ /go/src/build
RUN CGO_ENABLED=0 go build -ldflags "-s -w"

#FROM gcr.io/distroless/static-debian12
FROM debian:bullseye-slim

COPY --from=build /go/src/build/grafana-operator /usr/local/bin

ENTRYPOINT ["/usr/local/bin/grafana-operator"]