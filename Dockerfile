FROM golang:1.27.3-trixie@sha256:7b072c590a326b9d2a43b165ae247f6e8eafff32e1ad154ab97139e89f053be9 AS panel

WORKDIR /src
COPY panel/go.mod panel/go.sum ./
RUN go mod download
COPY panel/ ./
RUN go test ./... && CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/panel .

FROM cloudron/base:6.0.1@sha256:7b1e729960c3ca312d26e886917ba89483241bba0219ff292858e10d9d5768ee

# renovate: datasource=deb depName=mariadb-server
ARG MARIADB_VERSION=1:11.8.6-5ubuntu0.1

# links the GHCR package to the repo - https://docs.github.com/en/packages/learn-github-packages/connecting-a-repository-to-a-package
LABEL org.opencontainers.image.source=https://github.com/BrutalBirdie/MariaDB-Cloudron-App

RUN mkdir -p /app/code
WORKDIR /app/code

# replaces the base image's mysql-client; the data directory is created by start.sh in the persistent dir
RUN apt-get update && \
    apt-get install -y mariadb-server=${MARIADB_VERSION} mariadb-client=${MARIADB_VERSION} zstd && \
    rm -rf /var/cache/apt /var/lib/apt/lists /var/lib/mysql/*

RUN rm /etc/nginx/sites-enabled/* && \
    ln -sf /dev/stdout /var/log/nginx/access.log && \
    ln -sf /dev/stderr /var/log/nginx/error.log
COPY nginx/readonlyrootfs.conf /etc/nginx/conf.d/readonlyrootfs.conf
COPY nginx/app.conf /etc/nginx/sites-enabled/app.conf

COPY supervisor/* /etc/supervisor/conf.d/
COPY pam/mdbpanel /etc/pam.d/mdbpanel
RUN ln -sf /run/supervisord.log /var/log/supervisor/supervisord.log

COPY --from=panel /out/panel /app/code/panel
COPY start.sh backup.sh restore.sh mdbtenant.sh common.sh /app/pkg/

CMD [ "/app/pkg/start.sh" ]
