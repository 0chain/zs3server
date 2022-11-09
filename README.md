# Introduction
This module provides s3-compatible API to 0chain allocation using minio-gateway Interface.
User can set their access-key and secret-key before running zs3Server. So basically, zs3server is an http server that provides s3 compatible api so that clients that are already s3 compatible can easily communicates with 0chain allocationcan. Its just a plug and play.

# Run zs3-server
As a prerequisite to run MinIO ZCN gateway, you need 0chain credentials; wallet.json, config.yaml and allocation.txt.

## Build Binary
```
git clone git@github.com:0chain/zs3server.git
cd zs3server
go mod tidy
go build .
export MINIO_ROOT_USER=someminiouser
export MINIO_ROOT_PASSWORD=someminiopassword
./zs3server gateway zcn --configDir /path/to/config/dir
> Note: allocation and configDir both are optional. By default configDir takes ~/.zcn as configDir and if allocation is not provided in command then it will look for allocation.txt file in configDir directory.
```

## Test using MinIO Client `mc`

`mc` provides a modern alternative to UNIX commands such as ls, cat, cp, mirror, diff etc. It supports filesystems and Amazon S3 compatible cloud storage services.

### Configure `mc`
```
mc config host add zcn http://localhost:9000 miniouser miniopassword
mc ls zcn //List your buckets

[2017-02-22 01:50:43 PST]     0B user/
[2017-02-26 21:43:51 PST]     0B datasets/
[2017-02-26 22:10:11 PST]     0B assets/
```
