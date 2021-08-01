.PHONY: build cluster test

default: build

build:
	go build -o bin/swarm    github.com/69th-byte/sdexchain/cmd/swarm
	go build -o bin/tomo     github.com/69th-byte/sdexchain/cmd/tomo
	go build -o bin/bootnode github.com/69th-byte/sdexchain/cmd/bootnode

cluster: build
	scripts/boot-cluster.sh

test:
	go test -v github.com/69th-byte/sdexchain/swarm/...
