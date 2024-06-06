FROM golang:1.22-bullseye AS build

RUN apt update && update-ca-certificates --fresh && apt -y install build-essential

WORKDIR /go/src/build
COPY ./ /go/src/build
RUN CGO_ENABLED=0 go build -ldflags "-s -w" -buildvcs=false

FROM debian:bullseye-slim

RUN groupadd --gid 1000 1000 \
    && useradd --uid 1000 --gid 1000 -m 1000

USER 1000

COPY --from=build /go/src/build/grafana-operator /usr/local/bin

ENTRYPOINT ["/usr/local/bin/grafana-operator"]