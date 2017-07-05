FROM alpine:latest

RUN apk --no-cache add \
    ca-certificates \
    tini

ADD build/bin/eni-controller /bin/

ENTRYPOINT ["/sbin/tini", "--", "eni-controller"]
