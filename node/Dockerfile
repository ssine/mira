FROM golang:1.26.6-bookworm AS build

ARG MIRA_VERSION=0.11.1
ARG MIRA_COMMIT=unknown
ARG MIRA_BUILD_TIME=unknown

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
RUN link_flags="-s -w -X github.com/ssine/mira/node/internal/node.Version=${MIRA_VERSION} -X github.com/ssine/mira/node/internal/node.Commit=${MIRA_COMMIT} -X github.com/ssine/mira/node/internal/node.BuildTime=${MIRA_BUILD_TIME}" \
  && CGO_ENABLED=0 go build -trimpath -ldflags="$link_flags" -o /out/mira-node ./cmd/mira-node \
  && CGO_ENABLED=0 go build -trimpath -ldflags="$link_flags" -o /out/mira ./cmd/mira

FROM node:22-bookworm-slim

RUN apt-get update \
  && apt-get install --yes --no-install-recommends ca-certificates procps util-linux \
  && rm -rf /var/lib/apt/lists/* \
  && npm install --global @openai/codex@0.151.0
RUN install -d -o node -g node /home/node/.codex /home/node/.config/mira
COPY --from=build /out/mira-node /usr/local/bin/mira-node
COPY --from=build /out/mira /usr/local/bin/mira

USER node
WORKDIR /home/node
CMD ["mira-node"]
