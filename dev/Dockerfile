# Development toolchain for koroku.
#
# Everything needed to build and test the project lives in this image, so the
# host only needs Docker. Use it through bin/dev, for example:
#
#   bin/dev go test ./...
#   bin/dev sh -c 'cd web && pnpm install --frozen-lockfile && pnpm test'
#
# The container talks to the host's Docker daemon through the mounted socket
# (integration tests start real containers) and shares the host network so
# that ports published on 127.0.0.1 are reachable from the tests.

FROM node:26-slim AS node

FROM golang:1.27

ARG PNPM_VERSION=12.21.0
ARG RESTIC_VERSION=0.19.1

# fonts-noto-cjk gives Chromium glyphs for Japanese text, so screenshots of
# the management UI taken in this image are readable.
RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        bzip2 ca-certificates chromium curl fonts-noto-cjk git jq make shellcheck \
    && rm -rf /var/lib/apt/lists/*

# The frontend test runner (testem) launches "Chrome"; point that name at
# the distribution's Chromium.
RUN ln -s /usr/bin/chromium /usr/local/bin/google-chrome

# Docker CLI with the Compose and Buildx plugins (no daemon in the image).
RUN install -m 0755 -d /etc/apt/keyrings \
    && curl -fsSL https://download.docker.com/linux/debian/gpg -o /etc/apt/keyrings/docker.asc \
    && . /etc/os-release \
    && echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/docker.asc] https://download.docker.com/linux/debian ${VERSION_CODENAME} stable" \
        > /etc/apt/sources.list.d/docker.list \
    && apt-get update \
    && apt-get install -y --no-install-recommends docker-ce-cli docker-compose-plugin docker-buildx-plugin \
    && rm -rf /var/lib/apt/lists/*

# Node.js (copied from the official image) and pnpm for the Ember app.
COPY --from=node /usr/local/bin/node /usr/local/bin/node
COPY --from=node /usr/local/lib/node_modules /usr/local/lib/node_modules
RUN ln -s ../lib/node_modules/npm/bin/npm-cli.js /usr/local/bin/npm \
    && ln -s ../lib/node_modules/npm/bin/npx-cli.js /usr/local/bin/npx \
    && npm install --global "pnpm@${PNPM_VERSION}" \
    && npm cache clean --force

# restic for the backup integration tests. Debian's package is too old for
# the features the backup code uses, so the official release is installed and
# checked against the release's checksum file.
RUN cd /tmp \
    && curl -fsSLO "https://github.com/restic/restic/releases/download/v${RESTIC_VERSION}/restic_${RESTIC_VERSION}_linux_amd64.bz2" \
    && curl -fsSLO "https://github.com/restic/restic/releases/download/v${RESTIC_VERSION}/SHA256SUMS" \
    && grep " restic_${RESTIC_VERSION}_linux_amd64.bz2\$" SHA256SUMS | sha256sum -c - \
    && bunzip2 "restic_${RESTIC_VERSION}_linux_amd64.bz2" \
    && install -m 0755 "restic_${RESTIC_VERSION}_linux_amd64" /usr/local/bin/restic \
    && rm -f "restic_${RESTIC_VERSION}_linux_amd64" SHA256SUMS

# bin/dev runs the container with the caller's UID so that files written to
# the bind-mounted checkout are not owned by root. Caches and HOME therefore
# have to be writable by any user; they are backed by named volumes.
ENV HOME=/home/dev \
    GOPATH=/cache/go \
    GOCACHE=/cache/go-build \
    pnpm_config_store_dir=/cache/pnpm-store \
    CI=true
RUN mkdir -p /home/dev /cache/go /cache/go-build /cache/pnpm-store \
    && chmod 1777 /home/dev /cache/go /cache/go-build /cache/pnpm-store
ENV PATH=/cache/go/bin:$PATH
