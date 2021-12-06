module github.com/minio/minio

go 1.16

require (
	cloud.google.com/go/storage v1.10.0
	github.com/Azure/azure-pipeline-go v0.2.2
	github.com/Azure/azure-storage-blob-go v0.10.0
	github.com/Shopify/sarama v1.27.2
	github.com/VividCortex/ewma v1.1.1
	github.com/alecthomas/participle v0.2.1
	github.com/bcicen/jstream v1.0.1
	github.com/beevik/ntp v0.3.0
	github.com/bits-and-blooms/bloom/v3 v3.0.1
	github.com/cespare/xxhash/v2 v2.1.2
	github.com/cheggaaa/pb v1.0.29
	github.com/colinmarc/hdfs/v2 v2.2.0
	github.com/coredns/coredns v1.4.0
	github.com/coreos/go-oidc v2.1.0+incompatible
	github.com/cosnicolaou/pbzip2 v1.0.1
	github.com/dchest/siphash v1.2.1
	github.com/djherbis/atime v1.0.0
	github.com/dswarbrick/smart v0.0.0-20190505152634-909a45200d6d
	github.com/dustin/go-humanize v1.0.0
	github.com/eclipse/paho.mqtt.golang v1.3.0
	github.com/elastic/go-elasticsearch/v7 v7.12.0
	github.com/fatih/color v1.13.0
	github.com/go-ldap/ldap/v3 v3.2.4
	github.com/go-openapi/loads v0.20.2
	github.com/go-sql-driver/mysql v1.5.0
	github.com/golang-jwt/jwt/v4 v4.1.0
	github.com/gomodule/redigo v2.0.0+incompatible
	github.com/google/uuid v1.3.0
	github.com/gorilla/mux v1.8.0
	github.com/hashicorp/golang-lru v0.5.5-0.20210104140557-80c98217689d
	github.com/inconshreveable/mousetrap v1.0.0
	github.com/jcmturner/gokrb5/v8 v8.4.2
	github.com/json-iterator/go v1.1.12
	github.com/klauspost/compress v1.13.6
	github.com/klauspost/cpuid/v2 v2.0.9
	github.com/klauspost/pgzip v1.2.5
	github.com/klauspost/readahead v1.3.1
	github.com/klauspost/reedsolomon v1.9.14
	github.com/lib/pq v1.9.0
	github.com/miekg/dns v1.1.43
	github.com/minio/cli v1.22.0
	github.com/minio/console v0.12.5
	github.com/minio/csvparser v1.0.0
	github.com/minio/highwayhash v1.0.2
	github.com/minio/kes v0.14.0
	github.com/minio/madmin-go v1.1.16
	github.com/minio/mc v0.0.0-20211118223026-df75eed32e9e // indirect
	github.com/minio/minio-go/v7 v7.0.16
	github.com/minio/parquet-go v1.1.0
	github.com/minio/pkg v1.1.9
	github.com/minio/selfupdate v0.3.1
	github.com/minio/sha256-simd v1.0.0
	github.com/minio/simdjson-go v0.2.1
	github.com/minio/sio v0.3.0
	github.com/minio/zipindex v0.2.1
	github.com/mitchellh/go-homedir v1.1.0
	github.com/montanaflynn/stats v0.6.6
	github.com/nats-io/nats-server/v2 v2.3.2
	github.com/nats-io/nats.go v1.11.1-0.20210623165838-4b75fc59ae30
	github.com/nats-io/stan.go v0.8.3
	github.com/ncw/directio v1.0.5
	github.com/nsqio/go-nsq v1.0.8
	github.com/philhofer/fwd v1.1.2-0.20210722190033-5c56ac6d0bb9
	github.com/pierrec/lz4 v2.6.0+incompatible
	github.com/pkg/errors v0.9.1
	github.com/prometheus/client_golang v1.11.0
	github.com/prometheus/client_model v0.2.0
	github.com/prometheus/procfs v0.7.3
	github.com/rs/cors v1.7.0
	github.com/rs/dnscache v0.0.0-20210201191234-295bba877686
	github.com/secure-io/sio-go v0.3.1
	github.com/shirou/gopsutil/v3 v3.21.9
	github.com/streadway/amqp v1.0.0
	github.com/tinylib/msgp v1.1.7-0.20211026165309-e818a1881b0e
	github.com/valyala/bytebufferpool v1.0.0
	github.com/xdg/scram v0.0.0-20180814205039-7eeb5667e42c
	github.com/yargevad/filepathx v1.0.0
	github.com/zeebo/xxh3 v1.0.0
	go.etcd.io/etcd/api/v3 v3.5.0
	go.etcd.io/etcd/client/v3 v3.5.0
	go.uber.org/atomic v1.9.0
	go.uber.org/zap v1.19.1
	golang.org/x/crypto v0.0.0-20211117183948-ae814b36b871
	golang.org/x/oauth2 v0.0.0-20210819190943-2bc19b11175f
	golang.org/x/sys v0.0.0-20211124211545-fe61309f8881
	golang.org/x/time v0.0.0-20211116232009-f0f3c7e86c11
	google.golang.org/api v0.58.0
	gopkg.in/yaml.v2 v2.4.0
)

require (
	github.com/0chain/errors v1.0.3
	github.com/0chain/gosdk v1.3.4
	github.com/nats-io/nats-streaming-server v0.21.2 // indirect
)

replace (
	github.com/0chain/gosdk v1.3.4 => ../gosdk
	github.com/gomodule/redigo v2.0.0+incompatible => github.com/gomodule/redigo v1.8.5
)
