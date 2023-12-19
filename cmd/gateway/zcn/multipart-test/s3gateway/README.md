Implement the multipart upload for s3 gateway.

Start the gateway with the following command:
```
make
```

Upload a file `v1.mov` to bucket `vbkt` to the gateway server:

```
aws --endpoint-url http://localhost:8080 s3 cp v1.mov s3://vbkt
```

The `v1.mov` file should be greater than 100Mb to trigger the multipart upload. And we will see the file be saved to the `./store/vbkt/v1.mov` in the local storage.

This is an experiment to see the flow of how the aws s3 multipart upload works. The next step is to see how to forward the data to Zus storage via our SDK. 
