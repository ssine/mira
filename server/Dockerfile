FROM node:22-bookworm-slim

WORKDIR /app
COPY package.json package-lock.json ./
RUN npm ci --omit=dev
COPY db.mjs dynamic-tools.mjs node-channel.mjs node-registry.mjs server.mjs thread-store.mjs ./

USER node
CMD ["node", "server.mjs"]
