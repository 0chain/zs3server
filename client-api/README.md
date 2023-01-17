# Minio ClientAPI
![Main-architecture](../assets/main-struture.png)

This is the only component that can communicate with ZS2server and it is protected using the API key and API secret Key. 

It will be used also by the UI component to do al the required function like creating a bucket, deleting a bucket and putting an object, etc ....

If you are running the components using ``docker-compose`` the client API will be running on port ``3001`` so to access it from your local you can visit ``localhost:3001``

This component is also based on the minio Client SDK you can find it [here](https://github.com/minio/minio-go)

# How to use it?

There is a couple of function that has been already implemented but we can add more.

1. CreateBucket
2. listBuckets
3. listbucketsObjest
4. listObjects
5. getObject
6. putObject
7. deleteObject

There are two query parameters are needed for any api call

* AccessKey: same as the Zs3server root username
* SecretAccessKey: same as the Zs3server root password
## CreateBucket

This function will create a bucket for you and this bucket will be stored in 0chain allocation, it should be unique name

Example

```shell
curl -X GET -i 'http://localhost:3001/?action=createBucket&bucketName=${bucketname}'
```
Param:

* Action: "createBucket"
* bucketName: is the desired bucket name should be unique

Output:

```
{
  "Success": true,
  "Bucketname": "createdBucketName"
}
```

## listBuckets

This function to list all the avaliable buckets

Example:

```shell
curl -X GET -i 'http://localhost:3001/?action=listBuckets'
```

Param:

* ``action`` : "listBuckets"

Output:
* Array of buckets 

Example:

```
[
  {
    "BucketName": "root",
    "CreationDate": "2022-11-15T14:14:31Z"
  },
  {
    "BucketName": "test-zidan",
    "CreationDate": "2022-11-15T14:14:31Z"
  },
  {
    "BucketName": "test-zidan2",
    "CreationDate": "2022-11-17T07:57:17Z"
  }
]
```

## listbucketsObjest

This function will list all the bucket with it's objects


Example:

```shell
curl -X GET -i 'http://localhost:3001/?action=listBucketsObjects'
```

Param:

* ``action`` : "listBucketsObjects"

Output:

* array of buckets of objects

Example:
```
[
  {
    "BucketName": "test-zidan",
    "CreationTime": "2022-11-15T14:14:31Z",
    "BucketObjects": [
      {
        "Name": "OLXOTQAI.pdf",
        "LastModified": "2022-11-16T01:07:47Z"
      }
    ]
  },
  {
    "BucketName": "test-zidan2",
    "CreationTime": "2022-11-17T07:57:17Z",
    "BucketObjects": []
  }
]
```


## listObjects

This function will list all the objects for specific bucket

Example 

```shell
curl -X GET -i 'http://localhost:3001/?action=listObjects&bucketName=mybucketname'
```

Param:

* ``action`` : "listObjects"
* ``bucketName``: the bucket name that you want to list its objects

Output:

* Array of Objects

Example
```
[
  {
    "Name": "OLXOTQAI.pdf",
    "LastModified": "2022-11-16T01:07:47Z"
  }
]
```

## getObject

Returns a stream of the object data, this will be used to store the image then the user will be able to download it. 

Example 

```shell
curl -X GET -i 'http://localhost:3001/?action=getObject&bucketName=mybucketname&objectName=myobject'
```

Param:

* ``action`` : "getObject"
* ``bucketName``: the bucket name that you want to list its objects
* ``objectName``: the object name that you want to download it.

Output:

* Binary file will downloaded.

## putObject

This function is to upload object to zs3server 

Example:

```shell 
curl -X GET -F 'file=@path-to-file' http://localhost:3001/?action=putObject\&accessKey=rootroot\&secretAccessKey=rootroot\&bucketName=test2

```

Param:

* ``action`` : "putObject"
* ``bucketName``: the bucket name that you want to list its objects

Form:

* this API require a form filed with name ``file`` so it can upload this file to zs3server 

## removeObject

this API is to remove an object from zs3server


```shell 
curl -X GET -i 'http://localhost:3001/?action=removeObject&bucketName=mybucketname&objectName=${objectname}'

```

Param:

* ``action`` : "removeObject"
* ``bucketName``: the bucket name that you want to list its objects
* ``objectName``: the object name that you want to remove it.

Output:

```
{
  "Success": true,
  "ObjectName": "ObjectName"
}
```

## Admin APIs 

Admin apis are there to manage the users access, we are using the madmin-go sdk https://github.com/minio/madmin-go#AddUser

## What user can do
User can use this admin api to do couple of functionalities.

1. listUsers
2. addUser
3. removeUser
4. setUser

### ListUsers

To list all the users that has been created by the user, this doesn't consider the root user.

API parameter:

- ``action``: the action is case senstive and it should be ``listUsers``

- ``accessKey`` the root access key to be able to call the API

- ``secretAccessKey``: The root secret access key to b able to call the API.

API Response:

- ``Success``: if the API call success it will return ``200`` with a list of users
```json 
[
  {
    SecretKey: "" #user secret key
    AccessKey: "" #user access key
    Status: "" #user status enabled or disabled
    PolicyName: "" #policy name that has assigned to the user by default it is readwrite
    MemberOf: "" # member of which group if there is any
    UpdatedAt; "" # when the user has been updated
  }
]
```

- ``fail``; if the API fail will return ``500`` with the error message

Example:

```bash
curl -X GET -i http://localhost:3001/admin/?action=listUsers&accessKey=rootroot&secretAccessKey=rootroot
```

### addUser API

This API is implemented to add a new user by default this user will have ``readwrite`` access which will allow him to call any API except the admin APIs


API parameter:

- ``action``: the action is case senstive and it should be ``addUser``

- ``accessKey`` the root access key to be able to call the API

- ``secretAccessKey``: The root secret access key to b able to call the API.

- ``userAccessKey``: The new user access key that we want to add it.

- ``userSecretKey``: The new user secret key that we want to add it. 

API Response:

- ``Success``: If the request has been successed you will get the following response

```bash
{
  "Success":true, # status success true
  "AccessKey":"rootroot3" # the user access key that has been created
}
```

- ``Fail``; if the request fail you will get the following response 
```bash
{
  "error":"" #error message
}
```

Example:
```bash
curl -X GET -i http://localhost:3001/admin/?action=addUser&accessKey=rootroot&secretAccessKey=rootroot&userAccessKey=rootroot3&userSecretKey=rootroot3
```

## RemoveUser API

This API has been implemented to remove the user credentials from minio

API parameter:

- ``action``: the action is case senstive and it should be ``removeUser``

- ``accessKey`` the root access key to be able to call the API

- ``secretAccessKey``: The root secret access key to b able to call the API.

- ``userAccessKey``: The user access key that we want to remove it.

API Response:

- ``Success``: if the request has been success you will get the following response 
```json
{"Success":true}
```
- ``Fail``; if the request fail you will get the following response 
```bash
{
  "error":"" #error message
}
```

Example:

```bash
curl -X GET -i http://localhost:3001/admin/?action=removeUser&accessKey=rootroot&secretAccessKey=rootroot&userAccessKey=rootroot3
```

## SetUser API

This API has been implemented to update user ``secretKey`` and|or the user ``status`` (enabled, disabled) 

API parameter:

- ``action``: the action is case senstive and it should be ``setUser``

- ``accessKey`` the root access key to be able to call the API

- ``secretAccessKey``: The root secret access key to b able to call the API.

- ``userAccessKey``: The user access key that we want to change.
- ``userSecretKey``: The user secrey key that we want to change.
- ``status``: the user status that we want to change it, it accept one of these values (enabled, disabled)

API Response:

- ``Success``: if the request has been success you will get the following response 
```json
{"Success":true}
```
- ``Fail``; if the request fail you will get the following response 
```bash
{
  "error":"" #error message
}
```

Example:

```bash
curl -X GET -i http://localhost:3001/admin/?action=setUser&accessKey=rootroot&secretAccessKey=rootroot&userAccessKey=rootroot3&userSecretKey=rootroot3&status=disabled
```
