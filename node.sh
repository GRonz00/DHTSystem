#!/usr/bin/env bash

# $1: key.pem
# $2: Public IP Address
# $3: program name

set -euo pipefail

echo "Installing DHT in $2"
echo "Installing Golang Protocol Buffer compiler"
ssh -o StrictHostKeyChecking=no -i $1 ec2-user@$2 "sudo dnf install -y golang protobuf-compiler protobuf-devel"
echo "Installing gRPC Protocol Buffer extension"
ssh -o StrictHostKeyChecking=no -i $1 ec2-user@$2 "go install google.golang.org/protobuf/cmd/protoc-gen-go@latest"
ssh -o StrictHostKeyChecking=no -i $1 ec2-user@$2 "go install google.golang.org/grpc/cmd/protoc-gen-go-grpc@latest"
ssh -o StrictHostKeyChecking=no -i $1 ec2-user@$2 "mkdir -p dht/proto"
echo "Copying Application"
scp -o StrictHostKeyChecking=no -i $1 -r proto/*.proto ec2-user@$2:/home/ec2-user/dht/proto
scp -o StrictHostKeyChecking=no -i $1 -r node ec2-user@$2:/home/ec2-user/dht/node
scp -o StrictHostKeyChecking=no -i $1 -r bootstrap ec2-user@$2:/home/ec2-user/dht/bootstrap
scp -o StrictHostKeyChecking=no -i $1 go.mod ec2-user@$2:/home/ec2-user/dht/go.mod
scp -o StrictHostKeyChecking=no -i $1 go.sum ec2-user@$2:/home/ec2-user/dht/go.sum
scp -o StrictHostKeyChecking=no -i $1 dht.service_$2 ec2-user@$2:/home/ec2-user/dht/dht.service
echo "Moving systemd service"
ssh -o StrictHostKeyChecking=no -i $1 ec2-user@$2 "sudo mv /home/ec2-user/dht/dht.service /etc/systemd/system/dht.service"
echo "Compiling Protocol Buffers"
ssh -o StrictHostKeyChecking=no -i $1 ec2-user@$2 "cd dht && PATH=\$PATH:\$(go env GOPATH)/bin protoc --go_out=. --go_opt=paths=source_relative --go-grpc_out=. --go-grpc_opt=paths=source_relative proto/*.proto"
echo "Building Application"
ssh -o StrictHostKeyChecking=no -i $1 ec2-user@$2 "cd dht && go build -ldflags=\"-s -w\" -o build/node $3/main.go"
echo "Enabling and starting service"
ssh -o StrictHostKeyChecking=no -i $1 ec2-user@$2 "sudo systemctl daemon-reload"
ssh -o StrictHostKeyChecking=no -i $1 ec2-user@$2 "sudo systemctl enable --now dht.service"