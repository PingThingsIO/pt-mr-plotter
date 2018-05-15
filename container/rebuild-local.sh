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

##Choose how to Build the image

#Development builds use your local source code. Rebuild will also rebuild all the go binaries. To skip rebuilding these binaries and simply extend off your last build, use update-local.sh instead.

# We run docker build from the root of the project which allows the Dockerfile to access all files
# and no longer be restricted to just what exists in the ./container dir
pushd ..
docker build -f ./container/Dockerfile-DevRebuild --no-cache -t predictivegrid/mr-plotter:${ver} .

#2) The plain Dockerfile will clone from the PingThingsIO/pt-mr-plotter repo. You must have pushed any changes already and merged to master. Use this for publishing to production.
#It must be run in the context of the container directory
#docker build -f ./container/Dockerfile --no-cache -t predictivegrid/mr-plotter:${ver} .


#Uncomment any desired docker tags
# docker tag predictivegrid/mr-plotter:${ver} localdev/mr-plotter:latest

#Uncomment if you would like to push to dockerhub.
# docker push predictivegrid/mr-plotter:${ver}

