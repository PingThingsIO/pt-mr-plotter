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
docker build -f ./container/Dockerfile-Rebuild --no-cache -t pingthings/mrplotter:${ver} .
docker push pingthings/mrplotter:${ver}
#docker tag pingthings/dev-mrplotter:${ver} pingthings/dev-mrplotter:latest
#docker push pingthings/dev-mrplotter:latest
