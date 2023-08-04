# Introduction
This module provides s3-compatible API to 0chain allocation using minio-gateway Interface.
User can set their access-key and secret-key before running zs3Server. So basically, zs3server is an http server that provides s3 compatible api so that clients that are already s3 compatible can easily communicates with 0chain allocationcan. Its just a plug and play.

# Architecture 

![Main-architecture](./assets/main-struture.png)

There are three main components that will be installed in the customer server. 

1. ZS3Server is the main component which will communicate directly with 0chain allocation 

2. [LogSereach](/logsearchapi/README.md) API is the log component which will store the audit log from S3 server and It will be consumed using ZS3 API

3. [MinioClient](/client-api/README.md) is the component that will communicate directly to the zs3server and it is protected using access and secret key. 


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
./minio gateway zcn --configDir /path/to/config/dir
Note: allocation and configDir both are optional. By default configDir takes ~/.zcn as configDir and if allocation is not provided in command then it will look for allocation.txt file in configDir directory.
```

> If you want to debug on local you might want to build with `-gcflags="all=-N -l"` flag to view all the objects during debugging.

## Run using docker 

To build and run minio sevrer component in any machine you will need first to install docker and docker-compose 

1. Make sure docker and docker-compose in your machine

2. Make sure you have the allocation ready in the default folder ``~/.zcn``

3. Run docker-compose command like the following

```
docker-compose -f environment/docker-compose.yaml up -d
```

4. Now you can interact with the clint API follow this [doc](/client-api/README.md)

5. You can also interact with the logsearch API by following this [doc](/logsearchapi/README.md)

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
