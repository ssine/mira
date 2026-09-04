FROM node:22-bookworm-slim

WORKDIR /app
COPY --chown=node:node package.json package-lock.json ./
RUN npm ci --omit=dev --no-audit --no-fund
COPY --chown=node:node *.mjs ./
COPY --chown=node:node public ./public

USER node
CMD ["node", "server.mjs"]
