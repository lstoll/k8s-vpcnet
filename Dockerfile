FROM alpine:latest

RUN apk --no-cache add \
    ca-certificates \
    tini

ARG BINARY
ENV RUN_BINARY ${BINARY}

ADD build/bin/${BINARY} /bin/${BINARY}

# This shell hack sucks, thanks docker
ENTRYPOINT ["/sbin/tini", "--", "sh", "-c", "exec $RUN_BINARY"]
