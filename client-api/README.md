# Minio ClientAPI

This the only component that can communicate with ZS2server and it is protected using API key and API secret Key. 

It will be used also by the UI component to do al the required function like create bucket, delete bucket and put object etc ....

This component is also based on the minio Client SDK you can find it [here](https://github.com/minio/minio-go)

# How to use it?

There are a couple of function that has been already implementd but we can add more.

1. CreateBucket
2. listBuckets
3. listbucketsObjest
4. listObjects
5. getObject
6. putObject


## CreateBucket

This function will create a bucket for you and this bucket will be stored in 0chain allocation, it should be unique name

Example

```shell
curl -X GET -i 'http://localhost:3001/?action=CreateBucket' --data '{
"accessKey" : "rootroor",
"secretAccessKey" : "rootroot"
}'
```
Param:

* Action: is what are you trying to do
* bucketName: is the desired bucket name should be unique

Data:

* AccessKey: same as the Zs3server root username
* SecretAccessKey: same as the Zs3server root password

## listBuckets

This function to list all the avaliable buckets

Example:

```shell
curl -X GET -i 'http://localhost:3001/?action=listBuckets' --data '{
"accessKey" : "rootroor",
"secretAccessKey" : "rootroot"
}'
```

Param:

* ``action`` : "listnuckets"

Data:

* AccessKey: same as the Zs3server root username
* SecretAccessKey: same as the Zs3server root password

## listbucketsObjest

This function will list all the bucket with it's objects


Example:

```shell
curl -X GET -i 'http://localhost:3001/?action=listbucketsObjest' --data '{
"accessKey" : "rootroor",
"secretAccessKey" : "rootroot"
}'
```

Param:

* ``action`` : "listbucketsObjest"

Data:

* AccessKey: same as the Zs3server root username
* SecretAccessKey: same as the Zs3server root password


## listObjects

This function will list all the objects for specific bucket

Example 

```shell
curl -X GET -i 'http://localhost:3001/?action=listObjects&bucketName=mybucketname' --data '{
"accessKey" : "rootroor",
"secretAccessKey" : "rootroot"
}'
```

Param:

* ``action`` : "listbucketsObjest"
* ``bucketName``: the bucket name that you want to list its objects

Data:

* AccessKey: same as the Zs3server root username
* SecretAccessKey: same as the Zs3server root password

## getObject

Returns a stream of the object data, this will be used to store the image then the user will be able to download it. 

Example 

```shell
curl -X GET -i 'http://localhost:3001/?action=getObject&bucketName=mybucketname&objectName=myobject' --data '{
"accessKey" : "rootroor",
"secretAccessKey" : "rootroot"
}'
```

Param:

* ``action`` : "listbucketsObjest"
* ``bucketName``: the bucket name that you want to list its objects
* ``objectName``: the object name that you want to download it.

Data:

* AccessKey: same as the Zs3server root username
* SecretAccessKey: same as the Zs3server root password

## putObject

This function is to upload object to zs3server 

Example:

```shell 
curl -X GET -i 'http://localhost:3001/?action=putObject&bucketName=mybucketname&objectName=myobject' --data '{
"accessKey" : "rootroor",
"secretAccessKey" : "rootroot"
}'
```

Param:

* ``action`` : "listbucketsObjest"
* ``bucketName``: the bucket name that you want to list its objects
* ``objectName``: the object name that you want to download it.

Data:

* AccessKey: same as the Zs3server root username
* SecretAccessKey: same as the Zs3server root password


Note that this function later will change to ``POST`` request and it will work based on the upload form from The UI. 
