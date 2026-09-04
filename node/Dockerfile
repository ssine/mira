FROM node:22-bookworm-slim AS build

ARG TARGETARCH
ARG MIRA_VERSION=0.13.2

WORKDIR /src
# Build from the repository root after producing the matching native bundle.
# There is intentionally no plain-Go or system-OpenSSH fallback.
COPY VERSION ./VERSION
COPY node/openssh/verify-package.mjs node/openssh/stage-package.mjs ./node/openssh/
COPY node/openssh/out/linux-${TARGETARCH}/ /native/
RUN test "$(cat VERSION)" = "$MIRA_VERSION" \
  && node node/openssh/stage-package.mjs /native /out linux "$TARGETARCH"

FROM node:22-bookworm-slim

RUN apt-get update \
  && apt-get install --yes --no-install-recommends ca-certificates procps util-linux \
  && rm -rf /var/lib/apt/lists/*
RUN install -d -o node -g node /home/node/.codex /home/node/.config/mira
COPY --from=build /out/ /opt/mira/
RUN ln -s /opt/mira/mira-node /usr/local/bin/mira-node \
  && ln -s /opt/mira/mira /usr/local/bin/mira

USER node
WORKDIR /home/node
CMD ["mira-node"]
