# Сборка приложения
FROM golang:1.24-alpine AS application
ARG TAG
ADD . /bundle
WORKDIR /bundle
RUN apk --no-cache add ca-certificates
RUN \
    version=${TAG} && \
    echo "Building service. Version: ${version}" && \
    go build -ldflags "-X main.revision=${version}" -o /srv/app ./cmd/proxy/main.go

# Финальная сборка образа
FROM scratch
COPY --from=application /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/
COPY --from=application /srv /srv
ENV DEBUG=false
ENV OPTIONS_BUFFER_SIZE=65507
ENV OPTIONS_READ_DEADLINE=100
ENV OPTIONS_CLEANUP_INTERVAL=30
ENV OPTIONS_INACTIVE_TIMEOUT=5
ENV HOST_PORT=8080
ENV PROXY_PORT=9090
ENV PROXY_HOST=127.0.0.1
EXPOSE 8080
WORKDIR /srv
ENTRYPOINT ["/srv/app"]
