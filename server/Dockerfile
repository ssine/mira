FROM node:22-bookworm-slim

WORKDIR /app
COPY package.json package-lock.json ./
RUN npm ci --omit=dev
COPY admin-cli.mjs auth.mjs capability-service.mjs db.mjs dynamic-tools.mjs node-channel.mjs node-enrollment.mjs node-registry.mjs server.mjs thread-store.mjs ./
COPY public ./public

USER node
CMD ["node", "server.mjs"]
