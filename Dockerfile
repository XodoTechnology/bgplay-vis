# syntax=docker/dockerfile:1

FROM golang:1.24-bookworm AS build
WORKDIR /src
COPY go.mod ./
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -o /out/bgplay-vis ./cmd/bgplay-vis

FROM debian:bookworm-slim
RUN apt-get update \
 && apt-get install -y --no-install-recommends libcap2-bin ca-certificates \
 && rm -rf /var/lib/apt/lists/* \
 && useradd -r -u 10001 -s /usr/sbin/nologin bgplay
COPY --from=build /out/bgplay-vis /usr/local/bin/bgplay-vis
# bind :179 without running as root
RUN setcap 'cap_net_bind_service=+ep' /usr/local/bin/bgplay-vis
USER bgplay
EXPOSE 179 8080
ENTRYPOINT ["/usr/local/bin/bgplay-vis"]
CMD ["-bgp", ":179", "-web", ":8080"]
