# netns holder: owns the shared network namespace and its egress filter.
# The holder never runs user code, so Alpine is fine here — the glibc
# requirement (docs/decisions/0004-base-image-debian.md) binds the dev
# container only.
FROM alpine:3.22

RUN apk add --no-cache iptables iproute2 netcat-openbsd

COPY firewall.sh healthcheck.sh /usr/local/bin/
RUN chmod 755 /usr/local/bin/firewall.sh /usr/local/bin/healthcheck.sh

ENTRYPOINT ["/usr/local/bin/firewall.sh"]
