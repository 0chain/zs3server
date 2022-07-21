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