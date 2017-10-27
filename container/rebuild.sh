#!/bin/bash
set -ex

pushd ..
go build -v
ver=$(./mr-plotter -version)
popd
cp ../mr-plotter .
pushd ../tools/hardcodecert
go build -v
popd
pushd ../tools/setsessionkeys
go build -v
popd
cp ../tools/hardcodecert/hardcodecert .
cp ../tools/setsessionkeys/setsessionkeys .
docker build --no-cache -t pingthings/mrplotter:${ver} .
docker push pingthings/mrplotter:${ver}
docker tag pingthings/dev-mrplotter:${ver} pingthings/dev-mrplotter:latest
docker push pingthings/dev-mrplotter:latest
