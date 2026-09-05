FROM caddy:2-builder AS builder

COPY . /src
WORKDIR /src
RUN xcaddy build --with github.com/danmoz/caddy-lambda=.

FROM caddy:2-alpine

COPY --from=builder /src/caddy /usr/bin/caddy
COPY infra/Caddyfile /etc/caddy/Caddyfile

EXPOSE 80
