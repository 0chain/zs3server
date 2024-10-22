FROM golang:1.22-alpine3.18 as zbox_base

RUN apk add --update --no-cache linux-headers build-base git cmake bash perl grep py3-pip python3-dev curl

RUN apk upgrade
RUN apk del libstdc++ gmp-dev openssl-dev vips-dev
RUN apk add --update --no-cache --repository http://dl-cdn.alpinelinux.org/alpine/edge/main libstdc++ gmp-dev openssl-dev vips-dev 

# Install Herumi's cryptography
RUN cd /tmp && \
    wget -O - https://github.com/herumi/mcl/archive/refs/tags/v1.81.tar.gz  | tar xz && \
    wget -O - https://github.com/herumi/bls/archive/refs/tags/v1.35.tar.gz | tar xz && \
    mv mcl* mcl && \
    mv bls* bls

RUN cd /tmp && \
    make -C mcl -j $(nproc) lib/libmclbn256.so install && \
    cp mcl/lib/libmclbn256.so /usr/local/lib && \
    make MCL_DIR=$(pwd)/mcl -C bls -j $(nproc) install && \
    rm -R /tmp/mcl && \
    rm -R /tmp/bls