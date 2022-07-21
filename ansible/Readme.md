# Minio setup by ansible

Directory should be `~/zs3server/ansible`

1. copy your ansible server public key to minio server

2. Install tools

```
sudo apt update && sudo apt install python3-pip -y
```

```
sudo pip3 install -r requirements.txt
```

This bash script will setup minio -

```
bash minio_script.sh IP server_Username configDirectory miniousername miniopassword allocationID
```

example -
```
bash minio_script.sh 3.144.74.110 root $HOME/.zcn manali manalipassword 773dde936212cb60b312b1577a7a21aae4a4114b7ece242b8c2be5851b3656c4
```

Note - 
1. if `create_wallet: "yes"` then it will create wallet else won't create.

2. if `create_allocation: "yes"` then  it will `add token` , `register wallet` & `create allocation` else won't create it, can comment `allocation` if `create_allocation: "yes"`.

3. if `create_allocation: "no"` then you have to give `allocation` ID.