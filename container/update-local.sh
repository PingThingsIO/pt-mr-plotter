#!/bin/bash

#Rebuild the MRP golang code
pushd ..
GOOS=linux GOARCH=amd64 go build -v
mv pt-mr-plotter ./container

# Build a docker image based off of the last localdev/mrplotter with the new compiled go binary
# You'll have to edit rebuild-local.sh manually to create this base image the first time
docker build -f ./container/Dockerfile-DevUpdate --no-cache -t localdev/mrplotter:latest .

# Remove all the currently running docker containers based on the localdev/mrplotter image
docker ps -a | awk '{ print $1,$2 }' | grep localdev/mrplotter | awk '{print $1 }' | xargs -I {} docker rm -f {}

# Launch a new container based on the updated image
docker run --env ETCD_ENDPOINT="http://10.192.78.28:2379" --env BTRDB_ENDPOINTS="192.168.78.104:4410" -p 80:80 -p 443:443 --security-opt seccomp:unconfined localdev/mrplotter:latest
