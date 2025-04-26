package main

import (
	pb "DHTSystem/proto"
	"context"
	"fmt"
	"google.golang.org/grpc"
	"log"
	"math/rand"
	"net"
	"os"
	"sync"
)

type BootstrapServer struct {
	pb.UnimplementedBootstrapServiceServer
	nodes []string
	mu    sync.Mutex
}

func (b *BootstrapServer) FindActiveNode(ctx context.Context, address *pb.AddressList) (*pb.IPAddress, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.nodes) == 0 {
		b.nodes = append(b.nodes, address.Ad[0])
	}
	excluded := address.Ad
	excludedSet := make(map[string]struct{}, len(excluded))
	for _, addr := range excluded {
		excludedSet[addr] = struct{}{}
	}

	var validNodes []string

	// Filtra solo i nodi che non sono nella lista degli esclusi
	for _, node := range b.nodes {
		if _, found := excludedSet[node]; !found {
			validNodes = append(validNodes, node)
		}
	}

	// Se non ci sono nodi validi, ritorna errore
	if len(validNodes) == 0 {
		return nil, fmt.Errorf("no suitable node found")
	}

	// Scegli un nodo casuale tra quelli validi
	i := rand.Intn(len(validNodes))
	return &pb.IPAddress{Address: validNodes[i]}, nil
}
func (b *BootstrapServer) AddNode(ctx context.Context, address *pb.IPAddress) (*pb.IPAddress, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, node := range b.nodes {
		if node == address.Address {
			return nil, nil
		}
	}
	log.Println("Adding node:", address)

	b.nodes = append(b.nodes, address.Address)
	return &pb.IPAddress{Address: address.Address}, nil
}

func (b *BootstrapServer) RemoveNode(ctx context.Context, address *pb.IPAddress) (*pb.Bool, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for i, n := range b.nodes {
		if n == address.Address {
			b.nodes[i] = b.nodes[len(b.nodes)-1]
			b.nodes = b.nodes[:len(b.nodes)-1]
			return &pb.Bool{B: true}, nil
		}
	}
	return &pb.Bool{B: false}, nil
}
func main() {
	port := os.Getenv("PORT")
	listener, err := net.Listen("tcp", ":"+port)
	if err != nil {
		log.Fatalf("Errore nel creare il listener: %v", err)
	}

	grpcServer := grpc.NewServer()
	bootstrap := &BootstrapServer{
		nodes: make([]string, 0),
	}
	pb.RegisterBootstrapServiceServer(grpcServer, bootstrap)
	log.Println("Bootstrap server in ascolto su :" + port)

	if err := grpcServer.Serve(listener); err != nil {
		log.Fatalf("Errore nel servire il bootstrap: %v", err)
	}
}
