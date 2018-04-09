#!/bin/bash
set -ex

pushd ..
GOOS=linux GOARCH=amd64 go build -v
popd
pushd ../tools/version/main
go build -o getVersion
ver=$(./getVersion)
popd
pushd ../tools/hardcodecert
GOOS=linux GOARCH=amd64 go build -v
popd
pushd ../tools/setsessionkeys
GOOS=linux GOARCH=amd64 go build -v
popd
pushd ..

# We run docker build from the root of the project which allows the Dockerfile to access all files
# and no longer be restricted to just what exists in the ./container dir

##Choose how to Build the image

#1) The Dockerfile-Rebuild uses the code in the current directory.
docker build -f ./container/Dockerfile-Rebuild --no-cache -t predictivegrid/mr-plotter:${ver} .

#2) The plain Dockerfile will clone from the PingThingsIO/pt-mr-plotter repo. You must have pushed any changes already and merged to master.
#It must be run in the context of the container directory
# popd && docker build -f ./Dockerfile --no-cache -t predictivegrid/mr-plotter:${ver} .


#Uncomment any desired docker tags
# docker tag predictivegrid/mr-plotter:${ver} localdev/mr-plotter:latest

##Uncomment if you would like to push to dockerhub.
# docker push predictivegrid/mr-plotter:${ver}

