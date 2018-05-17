#Build stage to compile binaries
FROM golang:1.9.2 as builder

#Ensure dependencies
RUN go get -u github.com/golang/dep/...
RUN go get -u github.com/PingThingsIO/pt-mr-plotter/...
WORKDIR /go/src/github.com/PingThingsIO/pt-mr-plotter
RUN dep ensure

#Start building and moving output to ./buildbinaries
WORKDIR /buildbinaries

#Build pt-mr-plotter and move to ./buildbinaries
WORKDIR /go/src/github.com/PingThingsIO/pt-mr-plotter
RUN GOOS=linux GOARCH=amd64 go build -v
RUN mv pt-mr-plotter /buildbinaries

#Build hardcodecert and move to ./buildbinaries
WORKDIR /go/src/github.com/PingThingsIO/pt-mr-plotter/tools/hardcodecert
RUN GOOS=linux GOARCH=amd64 go build -v
RUN mv hardcodecert /buildbinaries

#Build setsessionkeys and move to ./buildbinaries
WORKDIR /go/src/github.com/PingThingsIO/pt-mr-plotter/tools/setsessionkeys
RUN GOOS=linux GOARCH=amd64 go build -v
RUN mv setsessionkeys /buildbinaries

#Final stage
FROM ubuntu:xenial

# we install etcdctl so that you can exec etcd commands inside the container
ENV ETCDCTL_VERSION v3.1.10
ENV ETCDCTL_ARCH linux-amd64
ENV GO_VERSION 1.9.2

RUN apt-get update && apt-get install -y vim wget net-tools git build-essential && \
    apt-get clean && rm -rf /var/lib/apt/lists/* /tmp/* /var/tmp/* && \
    wget -q -O- "https://github.com/coreos/etcd/releases/download/${ETCDCTL_VERSION}/etcd-${ETCDCTL_VERSION}-${ETCDCTL_ARCH}.tar.gz" |tar xfz - -C/tmp/ etcd-${ETCDCTL_VERSION}-${ETCDCTL_ARCH}/etcdctl && \
    mv /tmp/etcd-${ETCDCTL_VERSION}-${ETCDCTL_ARCH}/etcdctl /usr/local/bin/etcdctl
RUN wget -O /tmp/go.tar.gz https://storage.googleapis.com/golang/go${GO_VERSION}.linux-amd64.tar.gz && tar -xf /tmp/go.tar.gz -C /usr/local/ && rm /tmp/go.tar.gz && mkdir /srv/go
ENV PATH=/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin:/usr/local/go/bin:/srv/target:/srv/go/bin GOPATH=/srv/go
ENV GOTRACEBACK=all
ENV GOGC=40
RUN go get github.com/maruel/panicparse/cmd/pp
RUN mkdir -p $GOPATH/src/github.com/PingThingsIO && \
    cd $GOPATH/src/github.com/PingThingsIO && \
    git clone https://github.com/PingThingsIO/pt-mr-plotter.git && \
    cd pt-mr-plotter && \
    git checkout pingthings && \
    go get -d ./... && \
    go install .
ADD entrypoint.sh /
# Copy the binaries built from the first stage
COPY --from=builder /buildbinaries/* $GOPATH/bin/
RUN chmod a+x /entrypoint.sh
ENTRYPOINT ["/entrypoint.sh"]
