ARG GO_VERSION=1.23

# UI build stage
FROM node:20-alpine AS ui-builder
RUN apk add --no-cache make
COPY . /src/loki
WORKDIR /src/loki
RUN make -C pkg/dataobj/explorer/ui build

# Go build stage
FROM golang:${GO_VERSION} AS build
COPY . /src/loki
COPY --from=ui-builder /src/loki/pkg/dataobj/explorer/dist /src/loki/pkg/dataobj/explorer/dist
WORKDIR /src/loki
RUN make clean && make BUILD_IN_CONTAINER=false loki

# Final stage
FROM gcr.io/distroless/static:debug

COPY --from=build /src/loki/cmd/loki/loki /usr/bin/loki
COPY cmd/loki/loki-docker-config.yaml /etc/loki/local-config.yaml

SHELL [ "/busybox/sh", "-c" ]

RUN addgroup -g 10001 -S loki && \
    adduser -u 10001 -S loki -G loki
RUN mkdir -p /loki/rules && \
    mkdir -p /loki/rules-temp && \
    chown -R loki:loki /etc/loki /loki && \
    ln -s /busybox/sh /bin/sh
USER 10001
EXPOSE 3100
ENTRYPOINT [ "/usr/bin/loki" ]
CMD ["-config.file=/etc/loki/local-config.yaml"]
