#!/bin/bash
set -ex

pushd ..
GOOS=linux GOARCH=amd64 go build -v
mv pt-mr-plotter ~1
popd
pushd ../tools/version/main
go build -o getVersion
ver=$(./getVersion)
popd
pushd ../tools/hardcodecert
GOOS=linux GOARCH=amd64 go build -v
mv hardcodecert ~1
popd
pushd ../tools/setsessionkeys
GOOS=linux GOARCH=amd64 go build -v
mv setsessionkeys ~1
popd

#This file will rebuild all go binaries, and the plain Dockerfile will clone from the PingThingsIO/pt-mr-plotter repo. You must have pushed any changes already and merged to master. Use this for publishing to production.
docker build -f ./Dockerfile-Prod --no-cache -t predictivegrid/mr-plotter:${ver} .

#Uncomment any desired docker tags
#docker tag predictivegrid/mr-plotter:${ver} predictivegrid/mr-plotter:latest

#Uncomment if you would like to push to dockerhub.
#docker push predictivegrid/mr-plotter:${ver}

