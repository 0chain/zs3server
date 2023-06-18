FROM golang:1.20.4-alpine3.18 as zbox_base

RUN apk add --update --no-cache linux-headers build-base git cmake bash perl grep py3-pip python3-dev curl

RUN apk upgrade
# Install Herumi's cryptography
RUN apk del libstdc++ gmp-dev openssl-dev vips-dev
RUN apk add --update --no-cache --repository http://dl-cdn.alpinelinux.org/alpine/edge/main libstdc++ gmp-dev openssl-dev vips-dev 
