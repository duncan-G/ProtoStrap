FROM node:24-bookworm-slim
ENV NODE_ENV=production
WORKDIR /app
# There is no lockfile: package.json pins exact versions.
COPY package.json ./
RUN npm install --omit=dev --no-audit --no-fund && npm cache clean --force
COPY src/ ./src/
USER node
# Port 3000 serves the app, and port 9464 serves /metrics.
EXPOSE 3000 9464
CMD ["node", "src/server.js"]
