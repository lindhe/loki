ARG GO_VERSION=1.23
FROM golang:${GO_VERSION} AS build

COPY . /src/loki
WORKDIR /src/loki
RUN make clean && make BUILD_IN_CONTAINER=false logcli


FROM gcr.io/distroless/static:debug

COPY --from=build /src/loki/cmd/logcli/logcli /usr/bin/logcli
SHELL [ "/busybox/sh", "-c" ]
RUN ln -s /busybox/sh /bin/sh

ENTRYPOINT [ "/usr/bin/logcli" ]
