# LogSearch API Server for MinIO

![Main-architecture](../assets/main-struture.png)

LogSearch API where the Zs3sever will push the audit logs and it will be stored in postgresql

This API is being used by the official Minio-operatore which will be the default API when folks use k8s based deployment.


This Logsearch API is being compined with other 2 componets in one docker-compose file so it can be installed in the customer machine in one command.

## API Reference

The full API Reference is available [here](https://github.com/minio/operator/tree/master/logsearchapi).


Example 1: Filter and export request info logs of Put operations on the bucket `photos` in last 24 hours

```shell
curl -XGET -s \
   'http://logsearch:8080/api/query?q=reqinfo&timeAsc&export=ndjson&last=24h&fp=bucket:photos&fp=api_name:Put*' \
   --data-urlencode 'token=xxx' > output.ndjson
```

Example 2: Filter for the first 1000 raw audit logs of decommissioning operations in the last 1000 hours

```shell
curl -XGET -s \
   'http://logsearch:8080/api/query?q=raw&timeAsc&pageSize=1000&last=1000h&fp=api_name:*Decom*' \
   --data-urlencode 'token=xxx'
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

## sample api call to get the audit log.
```
http://localhost:8080/api/query?token=12345&q=raw&timeAsc&fp=api_name:Put*&pageSize=1000&last=1000h
```
## API reference

- API is coming from the minio operator for audit logs you can read more about it from here https://github.com/minio/operator/tree/master/logsearchapi


## How to interact with minio Server

```
https://documenter.getpostman.com/view/1747463/SzzheJEs
```

## To Do
- Write doc for logsearch api
- How to run the product using docker-compose
- How to access the url for an object
- Add more capabilities to the minioclient API
