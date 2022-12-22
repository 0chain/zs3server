# LogSearch API Server for MinIO

![Main-architecture](../assets/main-struture.png)

LogSearch API where the Zs3sever will push the audit logs and it will be stored in postgresql

This API is being used by the official Minio-operatore which will be the default API when folks use k8s based deployment.


This Logsearch API is being compined with other 2 componets in one docker-compose file so it can be installed in the customer machine in one command.

## Steps to build the logsearch API

We have built this API from the latest vesion of the official github repo you can find it [here](https://github.com/minio/operator/tree/master/logsearchapi) although the dockerfile you will find it here. 

1. Download the logsearch api from the main repo [here](https://github.com/minio/operator/tree/master/logsearchapi)

2. Attached the docker file from [here](./Dockerfile)

```
docker build -t logsearchapi .
``

4- Push the docker image to any docker registery and start using it from docker-compose file

## API Reference

The full API Reference is available [here](https://github.com/minio/operator/tree/master/logsearchapi).


Example 1: Filter for the first 1000 raw audit logs of decommissioning operations in the last 1000 hours

```shell
http://localhost:8080/api/query?token=12345&q=raw&timeDesc&fp=api_name:Put*&pageSize=1000&last=1000h
```

## Development setup

1. Start Postgresql server in container with logsearch api:

```shell
docker-compose up
```

3. Minio setup:


env variables:

```shell
export MINIO_AUDIT_WEBHOOK_ENDPOINT=http://localhost:8080/api/ingest?token=12345
export MINIO_AUDIT_WEBHOOK_AUTH_TOKEN="12345"  
export MINIO_AUDIT_WEBHOOK_ENABLE="on"    
export MINIO_ROOT_USER=adminadmin
export MINIO_ROOT_PASSWORD=adminadmin
export MINIO_BROWSER=OFF
./minio gateway zcn
```

```
http://localhost:8080/api/query?token=12345&q=raw&timeAsc&fp=api_name:Put*&pageSize=1000&last=1000h
```


## How to interact with minio Server

```
https://documenter.getpostman.com/view/1747463/SzzheJEs
```
