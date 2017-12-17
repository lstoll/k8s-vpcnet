FROM alpine:latest

RUN apk --no-cache add \
    ca-certificates \
    tini \
    iptables

ADD build/bin/vpcnet-daemon /bin/
ADD build/bin/vpcnet /cni/
ADD build/bin/loopback /cni/
ADD build/bin/ptp /cni/

ENTRYPOINT ["/sbin/tini", "--", "vpcnet-daemon"]
