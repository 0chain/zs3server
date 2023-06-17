FROM zbox_base as zbox_build

ENV SRC_DIR=/minio
ENV GO111MODULE=on

# Download the dependencies:
# Will be cached if we don't change mod/sum files

COPY ./go.mod $SRC_DIR/
COPY ./go.sum $SRC_DIR/

RUN cd $SRC_DIR && go mod download -x

COPY . $SRC_DIR/

WORKDIR /minio

RUN go build -o minio -buildvcs=false

# Copy the build artifact into a minimal runtime image:
FROM alpine:3.18

# RUN apk del libstdc++ gmp openssl vips
RUN apk add --update --no-cache --repository http://dl-cdn.alpinelinux.org/alpine/edge/main libstdc++ gmp openssl vips

COPY --from=zbox_build  /usr/local/lib/libmcl*.so \
    /usr/local/lib/libbls*.so \
    /usr/local/lib/


COPY --from=zbox_build /minio/minio /opt/bin/minio

COPY dockerscripts/docker-entrypoint.sh /usr/bin/docker-entrypoint.sh

# ENTRYPOINT ["/usr/bin/docker-entrypoint.sh"]

VOLUME ["/data"]

CMD ["/opt/bin/minio"]
