FROM node:22-alpine AS ui
WORKDIR /web
COPY web/package.json web/package-lock.json ./
RUN npm ci --no-audit --no-fund
COPY web/ ./
RUN npm run build
FROM golang:1.24-alpine AS backend
ARG VERSION=dev
WORKDIR /src
COPY go.mod go.sum *.go ./
COPY web/ ./web/
RUN go test ./... && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w -X main.version=${VERSION}" -o /botpages .
FROM alpine:3.21
ARG VERSION=dev
LABEL org.opencontainers.image.title="BotPages" org.opencontainers.image.version="${VERSION}" org.opencontainers.image.source="https://github.com/tainalo2/BotPages"
RUN addgroup -S app && adduser -S -G app app && mkdir /data && chown app:app /data
COPY --from=backend /botpages /usr/local/bin/botpages
COPY --from=ui /web/dist /admin
USER app
ENV BOTPAGES_ADDR=:8080 BOTPAGES_DATA=/data BOTPAGES_ADMIN_DIR=/admin
VOLUME /data
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/botpages"]
