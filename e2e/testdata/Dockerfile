FROM alpine:3.20 AS builder
ARG BIRD_VERSION
RUN apk add --no-cache gcc make flex bison ncurses-dev readline-dev linux-headers autoconf musl-dev curl
WORKDIR /src
RUN curl -sSL "https://github.com/CZ-NIC/bird/archive/refs/tags/v${BIRD_VERSION}.tar.gz" | tar xz --strip-components=1
RUN autoreconf -i 2>/dev/null || autoconf
RUN ./configure CFLAGS="-fcommon" && make -j$(nproc)

FROM alpine:3.20
RUN apk add --no-cache readline ncurses-libs
# source-built BIRD defaults its control socket to /usr/local/var/run/bird.ctl;
# that directory does not exist in the runtime image, so create it.
RUN mkdir -p /usr/local/var/run
COPY --from=builder /src/bird /usr/sbin/bird
COPY --from=builder /src/birdc /usr/sbin/birdc
CMD ["bird", "-c", "/etc/bird/bird.conf", "-f"]
