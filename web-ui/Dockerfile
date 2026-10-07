# Copyright 2024 AgentTier Authors.
# SPDX-License-Identifier: Apache-2.0

# Build stage
FROM public.ecr.aws/docker/library/node:20.19-alpine3.21@sha256:eb0b81f6eb35ec5ecd1cab181801f93a6314127580b416e6e290363b724e1686 AS builder

WORKDIR /app

# Install dependencies reproducibly (npm ci uses lockfile, not npm install)
COPY package.json package-lock.json ./
RUN npm ci

# Copy source and build
COPY . .
RUN npm run build

# Runtime stage
FROM public.ecr.aws/docker/library/nginx:1.27-alpine3.21@sha256:65645c7bb6a0661892a8b03b89d0743208a18dd2f3f17a54ef4b76fb8e2f2a10

# Copy built assets
COPY --from=builder /app/dist /usr/share/nginx/html

# Copy nginx configuration
COPY nginx.conf /etc/nginx/conf.d/default.conf

# Non-root user
RUN chown -R nginx:nginx /usr/share/nginx/html && \
    chown -R nginx:nginx /var/cache/nginx && \
    chown -R nginx:nginx /var/log/nginx && \
    chown -R nginx:nginx /etc/nginx/conf.d && \
    touch /var/run/nginx.pid && \
    chown -R nginx:nginx /var/run/nginx.pid

USER nginx

EXPOSE 8080

HEALTHCHECK --interval=30s --timeout=3s CMD wget -q --spider http://localhost/healthz || exit 1
