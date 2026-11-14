# docker-offline installer image: carries docker / docker-compose binaries plus
# systemd unit files, so a bundle can bootstrap docker on an offline Ubuntu host.
# All binaries come from the official base image (docker:<version>-dind ships
# dockerd/docker/containerd/containerd-shim-runc-v2/runc/docker-init/docker-proxy
# and the compose cli-plugin). The build performs no downloads beyond the base
# image itself — integrity and versions are guaranteed by the registry.
#
# Build (from this directory; multi-arch is REQUIRED — the packer extracts the
# materials for the target architecture via docker create + docker cp):
#   docker buildx build --platform linux/amd64,linux/arm64 \
#     -t ghcr.io/linuxsuren/compose-installer/docker-offline:27.5.1 --push .
#
# Other versions: override the base image and pick your own tag:
#   docker buildx build --platform linux/amd64,linux/arm64 \
#     --build-arg DOCKER_IMAGE=docker:29.8.0-dind \
#     -t ghcr.io/linuxsuren/compose-installer/docker-offline:29.8.0 --push .

# syntax=docker/dockerfile:1

ARG DOCKER_IMAGE=docker:27.5.1-dind
FROM ${DOCKER_IMAGE} AS bins
ARG DOCKER_IMAGE

# The final image is a pure file carrier (extracted by compose-installer with
# docker create + docker cp, no shell needed). ENTRYPOINT/CMD only exist to
# satisfy docker create — scratch images without a command are rejected — and
# are never actually executed.
FROM scratch
# Provenance: the base image actually used. Kept as ENV so this Dockerfile has
# zero RUN steps — buildx assembles files across architectures without
# qemu/binfmt.
ARG DOCKER_IMAGE
ENV BASE_IMAGE=${DOCKER_IMAGE}
ENTRYPOINT ["/offline/install.sh"]
CMD ["--yes"]
COPY --from=bins /usr/local/bin/docker                  /offline/docker/
COPY --from=bins /usr/local/bin/dockerd                 /offline/docker/
COPY --from=bins /usr/local/bin/containerd              /offline/docker/
COPY --from=bins /usr/local/bin/containerd-shim-runc-v2 /offline/docker/
COPY --from=bins /usr/local/bin/runc                    /offline/docker/
COPY --from=bins /usr/local/bin/docker-init             /offline/docker/
COPY --from=bins /usr/local/bin/docker-proxy            /offline/docker/
COPY --from=bins /usr/local/libexec/docker/cli-plugins/docker-compose /offline/bin/docker-compose
COPY systemd/ /offline/systemd/
COPY install.sh /offline/install.sh
COPY VERSIONS.env /offline/VERSIONS.env
