# zs3server - a no-code decentralized storage server

zs3server provides a no-code s3-compatible decentralized storage server on Züs allocation using a minio-gateway interface.

  - [Züs Overview](#züs-overview)
  - [Architecture](#architecture)
  - [Building zs3 server](#building-zs3-server)
  - [Running zs3 server](#running-zs3-server)
  - [Test MiniO client](#test-minio-client)
  - [Configure Minio client](#configure-minio-client)
  
## Züs Overview
[Züs](https://zus.network/) is a high-performance cloud on a fast blockchain offering privacy and configurable uptime. It is an alternative to traditional cloud S3 and has shown better performance on a test network due to its parallel data architecture. The technology uses erasure code to distribute the data between data and parity servers. Züs storage is configurable to provide flexibility for IT managers to design for desired security and uptime, and can design a hybrid or a multi-cloud architecture with a few clicks using [Blimp's](https://blimp.software/) workflow, and can change redundancy and providers on the fly.

For instance, the user can start with 10 data and 5 parity providers and select where they are located globally, and later decide to add a provider on-the-fly to increase resilience, performance, or switch to a lower cost provider.

Users can also add their own servers to the network to operate in a hybrid cloud architecture. Such flexibility allows the user to improve their regulatory, content distribution, and security requirements with a true multi-cloud architecture. Users can also construct a private cloud with all of their own servers rented across the globe to have a better content distribution, highly available network, higher performance, and lower cost.

[The QoS protocol](https://medium.com/0chain/qos-protocol-weekly-debrief-april-12-2023-44524924381f) is time-based where the blockchain challenges a provider on a file that the provider must respond within a certain time based on its size to pass. This forces the provider to have a good server and data center performance to earn rewards and income.

The [privacy protocol](https://zus.network/build) from Züs is unique where a user can easily share their encrypted data with their business partners, friends, and family through a proxy key sharing protocol, where the key is given to the providers, and they re-encrypt the data using the proxy key so that only the recipient can decrypt it with their private key.

Züs has ecosystem apps to encourage traditional storage consumption such as [Blimp](https://blimp.software/), a S3 server and cloud migration platform, and [Vult](https://vult.network/), a personal cloud app to store encrypted data and share privately with friends and family, and [Chalk](https://chalk.software/), a zero upfront cost permanent storage solution for NFT artists.

Other apps are [Bolt](https://bolt.holdings/), a wallet that is very secure with air-gapped 2FA split-key protocol to prevent hacks from compromising your digital assets, and it enables you to stake and earn from the storage providers; [Atlus](https://atlus.cloud/), a blockchain explorer and [Chimney](https://demo.chimney.software/), which allows anyone to join the network and earn using their server or by just renting one, with no prior knowledge required.

## Architecture 

![Main-architecture](./assets/main-struture.png)

There are three main components that will be installed in the customer server. 

1. ZS3Server is the main component that will communicate directly with Züs storage.

2. [LogSearch](/logsearchapi/README.md) API is the log component that will store the audit log from the S3 server and it will be consumed using ZS3 API

3. [MinioClient](/client-api/README.md) is the component that will communicate directly to the zs3server and it is protected using access and secret key. 

## Building zs3-server
Prerequisites to run MinIO ZCN gateway: 
- [A wallet.json created using zwalletcli](https://github.com/0chain/zwalletcli#creating-and-restoring-wallets)
- [Config.yaml](https://github.com/0chain/zboxcli/blob/staging/network/config.yaml) 
- [An allocation.txt created using zboxcli](https://github.com/0chain/zboxcli/tree/staging#create-new-allocation).

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

## Running zs3-server 

1. To build and run the minio server component you need to install [docker](https://www.docker.com/products/docker-desktop/). 

2. Run the docker-compose command inside the zs3server directory./
```
docker-compose -f environment/docker-compose.yaml up -d
```
3. Make sure allocation.txt file exist in the default folder ``~/.zcn``

## Test MinIO Client 

MinIO client `mc` provides a modern alternative to UNIX commands such as ls, cat, cp, mirror, diff, etc. It supports filesystems and Amazon S3-compatible cloud storage services. To interact with the client API follow this [doc](/client-api/README.md). You can also interact with the log search API by following this [doc.](/logsearchapi/README.md)

## Configure MinIO Client
```
mc config host add zcn http://localhost:9000 miniouser miniopassword
mc ls zcn //List your buckets

[2017-02-22 01:50:43 PST]     0B user/
[2017-02-26 21:43:51 PST]     0B datasets/
[2017-02-26 22:10:11 PST]     0B assets/
```
