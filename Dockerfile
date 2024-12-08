# wing/Dockerfile
FROM golang:1.22-bookworm as builder

WORKDIR /build

RUN apt-get update
RUN apt-get install -y git make llvm-15 clang-15

ENV CGO_ENABLED=0
ENV CLANG=clang-15
ARG VERSION=self-build

COPY . .

RUN make APPNAME=dae-wing VERSION=$VERSION

FROM alpine

WORKDIR /etc/dae-wing

RUN mkdir -p /usr/local/share/dae-wing
RUN wget -O /usr/local/share/dae-wing/geoip.dat https://github.com/v2rayA/dist-v2ray-rules-dat/raw/master/geoip.dat
RUN wget -O /usr/local/share/dae-wing/geosite.dat https://github.com/v2rayA/dist-v2ray-rules-dat/raw/master/geosite.dat
COPY --from=builder /build/dae-wing /usr/local/bin

EXPOSE 2023

CMD ["dae-wing"]
ENTRYPOINT ["dae-wing", "run", "-c", "."]
