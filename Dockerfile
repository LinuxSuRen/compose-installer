# syntax=docker/dockerfile:1
# compose-installer container image: single static binary on a small Alpine
# base with bash + ssh client (local install mode shells out to bash/scp) and
# CA certs. The delivery form hides the "Pack" tab (HIDE_PACK_TAB=1); run with
# -hide-pack-tab=false to restore it.

FROM golang:1.25-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
# web/dist is embedded at build time; CI builds the web UI before docker build.
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/compose-installer .

FROM alpine:3.21
RUN apk add --no-cache bash openssh-client ca-certificates tzdata
COPY --from=build /out/compose-installer /usr/local/bin/compose-installer
ENV HIDE_PACK_TAB=1
EXPOSE 8765
ENTRYPOINT ["compose-installer"]
CMD ["-addr", "0.0.0.0:8765", "-open-browser=false"]
