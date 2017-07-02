FROM alpine:latest

RUN apk --no-cache add \
    ca-certificates \
    tini

ARG BINARY
ENV RUN_BINARY ${BINARY}

ADD build/bin/${BINARY} /bin/${BINARY}

ENTRYPOINT ["/sbin/tini", "--"]
