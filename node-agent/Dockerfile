FROM node:22-bookworm-slim

RUN apt-get update \
  && apt-get install --yes --no-install-recommends procps util-linux \
  && rm -rf /var/lib/apt/lists/* \
  && npm install --global @openai/codex@0.151.0
RUN install -d -o node -g node /home/node/.codex
WORKDIR /app
COPY agent.mjs capabilities.mjs package.json ./

USER node
CMD ["node", "agent.mjs"]
