package main

import (
	pb "DHTSystem/proto"
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"google.golang.org/grpc"
	"log"
	"math"
	"math/rand"
	"net"
	"os"
	"strings"
	"time"
)

type Coordinates struct {
	MinX    float32
	MaxX    float32
	MinY    float32
	MaxY    float32
	CenterX float32
	CenterY float32
}
type Point struct {
	X float32
	Y float32
}
type Node struct {
	Coordinates Coordinates
	Address     string
}
type NodeServer struct {
	pb.UnimplementedNodeServiceServer
	coordinates          Coordinates
	address              string
	neighbours           map[string]Coordinates //Key=address mappa con le coordinate del vicino
	neighboursNeighbours map[string][]string    //Mappa con i vicini del vicino, usata in caso di failure per takeover
	resources            map[string]string      //La chiave è il nome della risorsa (La posizione è data dal hash del nome)
	tempResource         map[string]string      //Salvataggio temporaneo delle risorse di un nodo che se ne va finché la rete non si ri-bilanciata
}

func (n *NodeServer) contain(point Point) bool {
	return point.X >= n.coordinates.MinX && point.X <= n.coordinates.MaxX &&
		point.Y >= n.coordinates.MinY && point.Y <= n.coordinates.MaxY
}
func stringToPoint(s string) Point {
	hash := sha256.Sum256([]byte(s))         // Calcola SHA-256
	num := binary.BigEndian.Uint64(hash[:8]) // Usa i primi 8 byte come numero
	a := float32(num) / float32(^uint64(0))
	return Point{X: a, Y: a}
}
func (n *NodeServer) AddResource(ctx context.Context, resource *pb.Resource) (*pb.IPAddress, error) {
	point := stringToPoint(resource.Key)
	if n.contain(point) {
		n.resources[resource.Key] = resource.Value
		log.Printf("Added resource %s to node %s", resource.Key, n.address)
		return &pb.IPAddress{Address: n.address}, nil
	} else {
		nodeToRouting, err := n.routing(point)
		if err != nil {
			log.Println("Errore: Nessun vicino disponibile per il routing")
			return &pb.IPAddress{Address: n.address}, err
		}
		conn, err := grpc.Dial(nodeToRouting, grpc.WithInsecure())
		if err != nil {
			log.Printf("Impossibile connettersi al nodo %s: %v", nodeToRouting, err)
			return nil, err
		}
		defer conn.Close()
		nodeClient := pb.NewNodeServiceClient(conn)
		return nodeClient.AddResource(context.Background(), resource)
	}
}
func (n *NodeServer) DeleteResource(ctx context.Context, resourceKey *pb.Key) (*pb.IPAddress, error) {
	point := stringToPoint(resourceKey.K)
	if n.contain(point) {
		delete(n.resources, resourceKey.K)
		return &pb.IPAddress{Address: n.address}, nil
	} else {
		nodeToRouting, err := n.routing(point)
		if err != nil {
			log.Println("Errore: Nessun vicino disponibile per il routing")
			return &pb.IPAddress{Address: n.address}, err
		}
		conn, err := grpc.Dial(nodeToRouting, grpc.WithInsecure())
		if err != nil {
			log.Printf("Impossibile connettersi al nodo %s: %v", nodeToRouting, err)
			return nil, err
		}
		defer conn.Close()
		nodeClient := pb.NewNodeServiceClient(conn)
		return nodeClient.DeleteResource(context.Background(), resourceKey)
	}
}
func (n *NodeServer) GetResource(ctx context.Context, resourceKey *pb.Key) (*pb.Resource, error) {
	point := stringToPoint(resourceKey.K)
	if n.contain(point) {
		if _, ok := n.resources[resourceKey.K]; ok {
			return &pb.Resource{Key: resourceKey.K, Value: n.resources[resourceKey.K]}, nil
		}
		return nil, errors.New("non esiste la risorsa")
	} else {
		nodeToRouting, err := n.routing(point)
		if err != nil {
			log.Println("Errore: Nessun vicino disponibile per il routing")
			return nil, err
		}
		conn, err := grpc.Dial(nodeToRouting, grpc.WithInsecure())
		if err != nil {
			log.Printf("Impossibile connettersi al nodo %s: %v", nodeToRouting, err)
			return nil, err
		}
		defer conn.Close()
		nodeClient := pb.NewNodeServiceClient(conn)
		return nodeClient.GetResource(context.Background(), resourceKey)
	}
}
func calculateDistance(point1 Point, point2 Point) float64 {
	return math.Pow(float64(point1.X-point2.X), 2) + math.Pow(float64(point1.Y-point2.Y), 2)
}
func convertCoordinateFromMessage(coordinate *pb.Coordinate) Coordinates {
	return Coordinates{
		MinX:    coordinate.MinX,
		MinY:    coordinate.MinY,
		MaxX:    coordinate.MaxX,
		MaxY:    coordinate.MaxY,
		CenterX: coordinate.CenterX,
		CenterY: coordinate.CenterY,
	}
}
func convertCoordinateToMessage(coordinate Coordinates) *pb.Coordinate {
	return &pb.Coordinate{
		MinX:    coordinate.MinX,
		MaxX:    coordinate.MaxX,
		MinY:    coordinate.MinY,
		MaxY:    coordinate.MaxY,
		CenterX: coordinate.CenterX,
		CenterY: coordinate.CenterY,
	}
}
func convertNodeInformationFromMessage(node *pb.NodeInformation) Node {
	return Node{
		Coordinates: convertCoordinateFromMessage(node.Coordinate),
		Address:     node.OriginNode.GetAddress(),
	}
}
func (n *NodeServer) routing(point Point) (string, error) {
	if len(n.neighbours) == 0 {
		log.Println("Errore: Nessun vicino disponibile per il routing")
		return "", errors.New("nessun vicino disponibile per il routing")
	}

	minDistance := math.MaxFloat64 // Inizializza con il massimo valore possibile
	var bestNode string

	for k, v := range n.neighbours {
		nodePoint := Point{X: v.CenterX, Y: v.CenterY}
		dist := calculateDistance(nodePoint, point)
		if dist < minDistance {
			minDistance = dist
			bestNode = k
		}
	}

	return bestNode, nil
}

func (n *NodeServer) removeOldNeighbours() {
	for neighbour, coordinates := range n.neighbours {
		if !(                                                                                  //se non è un vicino lo rimuovo
		((n.coordinates.MaxX == coordinates.MinX || n.coordinates.MinX == coordinates.MaxX) && //è un vicino se lo tocca a destra o sinistra
			(coordinates.CenterY <= n.coordinates.MaxY && coordinates.CenterY >= n.coordinates.MinY)) || //e allo stesso tempo è centrato rispetto alle y (non considero vicini quelli che toccano solo un punto, con pochi nodi sarebbero quasi tutti vicini)
			(n.coordinates.MaxY == coordinates.MinY || n.coordinates.MinY == coordinates.MaxY) && //stesso ragionamento assi invertite
				(coordinates.CenterX <= n.coordinates.MaxX && coordinates.CenterX >= n.coordinates.MinX)) || neighbour == n.address {
			_, ok := n.neighbours[neighbour]
			if ok {
				delete(n.neighbours, neighbour)
				delete(n.neighboursNeighbours, neighbour)
			}
			log.Printf("Ho rimosso il nodo %s come vicino", neighbour)
		}
	}
}

func (n *NodeServer) SplitZone(ctx context.Context, newNode *pb.NewNodeInformation) (*pb.InformationToAddNode, error) {
	log.Printf("Divido la zona con %s", newNode.Address)
	point := Point{newNode.Point.X, newNode.Point.Y}
	newCoordinate := Coordinates{
		MinX: n.coordinates.MinX, MaxX: n.coordinates.MaxX,
		MinY: n.coordinates.MinY, MaxY: n.coordinates.MaxY,
	}

	// Dividi lo spazio in base alla dimensione maggiore
	if (n.coordinates.MaxY - n.coordinates.MinY) <= (n.coordinates.MaxX - n.coordinates.MinX) { //Divido sulle X essendo più grande
		if n.coordinates.CenterX < point.X { //Il vecchio punto si prende la metà dove non casca il punto
			n.coordinates.MaxX = n.coordinates.CenterX
			newCoordinate.MinX = n.coordinates.CenterX

		} else {
			n.coordinates.MinX = n.coordinates.CenterX
			newCoordinate.MaxX = n.coordinates.CenterX
		}
		n.coordinates.CenterX = (n.coordinates.MaxX-n.coordinates.MinX)/2 + n.coordinates.MinX
	} else {
		if n.coordinates.CenterY < point.Y {
			n.coordinates.MaxY = n.coordinates.CenterY
			newCoordinate.MinY = n.coordinates.CenterY
		} else {
			n.coordinates.MinY = n.coordinates.CenterY
			newCoordinate.MaxY = n.coordinates.CenterY
		}
		n.coordinates.CenterY = (n.coordinates.MaxY-n.coordinates.MinY)/2 + n.coordinates.MinY
	}
	newCoordinate.CenterX = (newCoordinate.MaxX-newCoordinate.MinX)/2 + newCoordinate.MinX
	newCoordinate.CenterY = (newCoordinate.MaxY-newCoordinate.MinY)/2 + newCoordinate.MinY

	log.Printf("Le mie nuove coordinate sono x_min: %f, x_max: %f, y_min: %f, y_max: %f", n.coordinates.MinX, n.coordinates.MaxX, n.coordinates.MinY, n.coordinates.MaxY)
	resourceOfNeighbour := make([]*pb.Resource, 0)
	for key, value := range n.resources {
		if !n.contain(stringToPoint(key)) {
			resourceOfNeighbour = append(resourceOfNeighbour, &pb.Resource{Key: key, Value: value})
			delete(n.resources, key)
		}
	}
	//create message to new node
	neighboursCopy := make([]*pb.NodeInformation, 0, len(n.neighbours)) // Slice con capacità pre-allocata, ma vuota
	for neighbour, coordinate := range n.neighbours {
		neighboursCopy = append(neighboursCopy, &pb.NodeInformation{
			Coordinate: convertCoordinateToMessage(coordinate),
			OriginNode: &pb.IPAddress{Address: neighbour},
		})
	}

	n.neighbours[newNode.Address.GetAddress()] = newCoordinate
	n.removeOldNeighbours()
	log.Printf("my new neighbours are %v", n.neighbours)

	originNode := &pb.NodeInformation{
		Coordinate: convertCoordinateToMessage(n.coordinates),
		OriginNode: &pb.IPAddress{Address: n.address},
	}
	return &pb.InformationToAddNode{NewCoordinate: convertCoordinateToMessage(newCoordinate), OldNode: originNode, NeighboursOldNode: neighboursCopy, Resources: resourceOfNeighbour}, nil
}
func volumeCoordinate(coordinate Coordinates) float32 {
	return (coordinate.MaxX - coordinate.MinX) * (coordinate.MaxY - coordinate.MinY)
}
func (n *NodeServer) AddNode(ctx context.Context, newNode *pb.NewNodeInformation) (*pb.InformationToAddNode, error) {
	log.Printf("Richiesta di aggiunta nodo: %s nel punto (%f, %f)", newNode.Address.Address, newNode.Point.X, newNode.Point.Y)
	point := Point{newNode.Point.X, newNode.Point.Y}
	if n.contain(point) {
		log.Printf("E' di mia competenza")
		//Per maggiore uniformità il nuovo nodo divide la zona con il vicino con la zona più grande
		maxZone := float32(0)
		var maxNeighbor string
		for neig, coordinate := range n.neighbours {
			if volumeCoordinate(coordinate) > maxZone {
				maxZone = volumeCoordinate(coordinate)
				maxNeighbor = neig
			}
		}
		if volumeCoordinate(n.coordinates) >= maxZone { //Il nodo è più grande dei vicini quindi divide direttamente le sue coordinate
			return n.SplitZone(context.Background(), newNode)
		} else {
			log.Printf("Il vicino %s ha una zona più grande, faccio dividere a lui la zona", maxNeighbor)
			conn, err := grpc.Dial(maxNeighbor, grpc.WithInsecure())
			if err != nil {
				log.Printf("Non riesco a connettermi al nodo con zona piu grande, %v", err)
				return n.SplitZone(context.Background(), newNode)
			}
			defer conn.Close()

			client := pb.NewNodeServiceClient(conn)
			return client.SplitZone(context.Background(), newNode)

		}

	} else {
		log.Printf("Non è di mia competenza")

		nodeToRouting, err := n.routing(point)
		if err != nil {
			log.Println("Errore: nessun nodo trovato per il routing")
			return nil, err
		}
		log.Printf("lo mando al nodo %s", nodeToRouting)

		conn, err := grpc.Dial(nodeToRouting, grpc.WithInsecure())
		if err != nil {
			log.Printf("Impossibile connettersi al nodo %s: %v", nodeToRouting, err)
			return nil, err
		}
		defer conn.Close()
		nodeClient := pb.NewNodeServiceClient(conn)
		return nodeClient.AddNode(context.Background(), newNode)
	}

}
func (n *NodeServer) UnionZone(ctx context.Context, info *pb.InformationToAddNode) (*pb.Bool, error) {
	for _, r := range info.Resources {
		n.resources[r.Key] = r.Value
	}
	cooOldNode := convertCoordinateFromMessage(info.NewCoordinate)
	if n.coordinates.MinX > cooOldNode.MinX {
		n.coordinates.MinX = cooOldNode.MinX
	}
	if n.coordinates.MinY > cooOldNode.MinY {
		n.coordinates.MinY = cooOldNode.MinY
	}
	if n.coordinates.MaxX < cooOldNode.MaxX {
		n.coordinates.MaxX = cooOldNode.MaxX
	}
	if n.coordinates.MaxY < cooOldNode.MaxY {
		n.coordinates.MaxY = cooOldNode.MaxY
	}
	for _, neighbour := range info.NeighboursOldNode {
		n.neighbours[neighbour.OriginNode.Address] = convertCoordinateFromMessage(neighbour.Coordinate)
	}
	n.removeOldNeighbours()

	return nil, nil
}
func (n *NodeServer) DeepSearch(ctx context.Context, nodes *pb.HeartBeatMessage) (*pb.HeartBeatMessage, error) {
	minZone := float32(math.MaxFloat32)
	var minNeighbour string
	for neighbour, coordinate := range n.neighbours {
		//Cerchi sulla dimesione dove ti sei diviso
		//siccome divido prima sulle x e poi sulle y, se è un quadrato l'ultima divisione è stata sulle y
		if (n.coordinates.MaxX - n.coordinates.MinX) == (n.coordinates.MaxY - n.coordinates.MinY) {
			if (n.coordinates.MaxX >= coordinate.MaxX) && (n.coordinates.MinX <= coordinate.MinX) {
				if volumeCoordinate(n.coordinates) == volumeCoordinate(coordinate) {
					//Ho trovato il fratello e ha la stessa dimensione

					return &pb.HeartBeatMessage{Nodes: append(nodes.Nodes,
						&pb.NodeInformation{OriginNode: &pb.IPAddress{Address: neighbour},
							Coordinate: convertCoordinateToMessage(coordinate)},
						&pb.NodeInformation{OriginNode: &pb.IPAddress{Address: n.address},
							Coordinate: convertCoordinateToMessage(n.coordinates)},
					)}, nil
				}
				if volumeCoordinate(coordinate) < minZone {
					minNeighbour = neighbour
				}
			}
		} else {
			if (n.coordinates.MaxY >= coordinate.MaxY) && (n.coordinates.MinY <= coordinate.MinY) {
				if volumeCoordinate(n.coordinates) == volumeCoordinate(coordinate) {
					//Ho trovato il fratello e ha la stessa dimensione

					return &pb.HeartBeatMessage{Nodes: append(nodes.Nodes,
						&pb.NodeInformation{OriginNode: &pb.IPAddress{Address: neighbour},
							Coordinate: convertCoordinateToMessage(coordinate)},
						&pb.NodeInformation{OriginNode: &pb.IPAddress{Address: n.address},
							Coordinate: convertCoordinateToMessage(n.coordinates)},
					)}, nil
				}
				if volumeCoordinate(coordinate) < minZone {
					minNeighbour = neighbour

				}
			}
		}
	}
	client, err := createClient(minNeighbour)
	if err != nil {
		log.Printf("Errore nel connettersi a %s nella deepSearch: %v", minNeighbour, err)
		//todo cosa fare se vanno male le cose nella deep search
	}
	finalList, err := client.DeepSearch(ctx, nodes)
	if err != nil {
		log.Printf("Deep search andata male %v", err)
	}
	return &pb.HeartBeatMessage{Nodes: append(finalList.Nodes, &pb.NodeInformation{OriginNode: &pb.IPAddress{Address: n.address}, Coordinate: convertCoordinateToMessage(n.coordinates)})}, nil
}
func splitZones(nodes []Node) {
	x_min := float32(math.MaxFloat32)
	x_max := float32(0)
	y_min := float32(math.MaxFloat32)
	y_max := float32(0)
	//Ricostruisci zona piu grande da dividere
	for _, node := range nodes {
		if node.Coordinates.MaxY > y_max {
			y_max = node.Coordinates.MaxY
		}
		if node.Coordinates.MinY < y_min {
			y_min = node.Coordinates.MinY
		}
		if node.Coordinates.MaxX > x_max {
			x_max = node.Coordinates.MaxX
		}
		if node.Coordinates.MinX < x_min {
			x_min = node.Coordinates.MinX
		}
	}
	//Scegli nuove coordinate zone e mandale alle rispettive zone, fai in maniera sensata invio delle risorse e heartbeat
}
func (n *NodeServer) EntrustResources(ctx context.Context, info *pb.Resources) (*pb.Bool, error) {
	//TODO PROVA DEEP SEARCH
	unionNodes, err := n.DeepSearch(context.Background(), &pb.HeartBeatMessage{})
	if err != nil {
		log.Printf("La deep search è andata male entrust resours, %v", err)
		//todo cosa fare se va male deep search
	}
	totalZones := float32(0)
	nodesToUnion := make([]Node, 0, len(unionNodes.Nodes))
	for _, node := range unionNodes.Nodes {
		totalZones += volumeCoordinate(convertCoordinateFromMessage(node.Coordinate))
		nodesToUnion = append(nodesToUnion, Node{Coordinates: convertCoordinateFromMessage(node.Coordinate), Address: node.OriginNode.Address})
	}
	if totalZones == volumeCoordinate(convertCoordinateFromMessage(info.CoordinateOldNode)) {
		splitZones(nodesToUnion)
	}

	//altrimenti
	//se volume zona unita == zona nodo da rimuovere OK unisci con zona del nodo da eliminare e spartisci la zona tra i nodi
	//altrimenti cerca fratello zona unita
	for _, resource := range info.Resources {
		n.tempResource[resource.Key] = resource.Value
	}
	return nil, nil
}
func createClient(address string) (pb.NodeServiceClient, error) {
	conn, err := grpc.Dial(address, grpc.WithInsecure())
	if err != nil {
		log.Printf("Impossibile connettersi al nodo %s: %v", address, err)
		return nil, err
	}

	return pb.NewNodeServiceClient(conn), nil
}
func (n *NodeServer) removeNode() {
	if len(n.neighbours) == 0 {
		log.Fatalf("Non ho vicini a cui affidare le risorse")
		return
	}
	minVolume := float32(math.MaxFloat32) //se non trovo un nodo adeguato affido temporanreamente a quello con la zona più piccola
	resourcesToSend := make([]*pb.Resource, 0)
	for key, value := range n.resources {
		resourcesToSend = append(resourcesToSend, &pb.Resource{Key: key, Value: value})
		delete(n.resources, key)
	}
	var sendTo string

	for neighbour, coordinates := range n.neighbours {
		if volumeCoordinate(n.coordinates) == volumeCoordinate(coordinates) {
			//create message to new node
			neighboursCopy := make([]*pb.NodeInformation, 0, len(n.neighbours))
			for ne, coordinate := range n.neighbours {
				neighboursCopy = append(neighboursCopy, &pb.NodeInformation{
					Coordinate: convertCoordinateToMessage(coordinate),
					OriginNode: &pb.IPAddress{Address: ne},
				})
			}
			client, err := createClient(neighbour)
			if err != nil {
				log.Printf("Impossibile unire zone, %v", err)
			} else {
				_, err := client.UnionZone(context.Background(), &pb.InformationToAddNode{NewCoordinate: convertCoordinateToMessage(n.coordinates), Resources: resourcesToSend, NeighboursOldNode: neighboursCopy})
				if err != nil {
					log.Printf("Impossibile unire zone, %v", err)
				} else {
					bootstrapAddress := "bootstrap:50051"

					conn, err := grpc.Dial(bootstrapAddress, grpc.WithInsecure())
					if err != nil {
						log.Fatalf("Impossibile connettersi al bootstrap node %s: %v", bootstrapAddress, err)
					}
					defer conn.Close()

					client := pb.NewBootstrapServiceClient(conn)
					client.RemoveNode(context.Background(), &pb.IPAddress{Address: n.address})
					return
				}
			}

		}
		volume := volumeCoordinate(coordinates)
		if volume < minVolume {
			minVolume = volume
			sendTo = neighbour
		}
	}
	client, err := createClient(sendTo)
	if err != nil {
		log.Printf("Impossibile connettersi al nodo %s, %v", sendTo, err)
		delete(n.neighbours, sendTo)
		n.removeNode()
	} else {
		_, err := client.EntrustResources(context.Background(), &pb.Resources{Resources: resourcesToSend})
		if err != nil {
			log.Printf("Impossibile affidare risorse al nodo %s, %v", sendTo, err)
			delete(n.neighbours, sendTo)
			n.removeNode()
		}
	}
}

func (n *NodeServer) HeartBeat(ctx context.Context, neighbourNeighbour *pb.HeartBeatMessage) (*pb.HeartBeatMessage, error) { //  Quando ricevi heartbeat aggiorna vicini e le loro coordinate
	log.Printf("HeartBeat")
	addressNeighboursNeighbour := make([]string, 0, len(neighbourNeighbour.Nodes))
	for _, neighbourMessage := range neighbourNeighbour.Nodes {
		neighbour := convertNodeInformationFromMessage(neighbourMessage)
		if neighbour.Address != neighbourNeighbour.FromWho {
			addressNeighboursNeighbour = append(addressNeighboursNeighbour, neighbour.Address)
		}
		n.neighbours[neighbour.Address] = neighbour.Coordinates
	}
	n.neighboursNeighbours[neighbourNeighbour.FromWho] = addressNeighboursNeighbour
	n.removeOldNeighbours()
	log.Printf("i vicini a seguito heartbeat %v", n.neighbours)
	log.Printf("i vicini di %s sono: %v", neighbourNeighbour.FromWho, n.neighboursNeighbours[neighbourNeighbour.FromWho])

	return neighbourNeighbour, nil
}

// Periodicamente e quando viene aggiunto come nodo, il nodo manda a tutti i suoi vicini le sue coordinate e i vicini (aggiornamento soft)
func (n *NodeServer) sendHeartBeatAllNeighbours() {
	for {
		log.Printf("Sto facendo il send")
		//Crea messaggio con tutti i vicini e te stesso
		heartbeat := make([]*pb.NodeInformation, 0, len(n.neighbours)+1) // +1 per includere il nodo stesso

		// Aggiungi i vicini
		for neighbour, coordinates := range n.neighbours {
			heartbeat = append(heartbeat, &pb.NodeInformation{
				OriginNode: &pb.IPAddress{Address: neighbour},
				Coordinate: convertCoordinateToMessage(coordinates),
			})
		}

		// Aggiungi il nodo stesso
		heartbeat = append(heartbeat, &pb.NodeInformation{
			OriginNode: &pb.IPAddress{Address: n.address},
			Coordinate: convertCoordinateToMessage(n.coordinates),
		})

		//Invia a tutti i vicini heartbeat
		for neighbour := range n.neighbours {
			conn, err := grpc.Dial(neighbour, grpc.WithInsecure())
			if err != nil {
				log.Printf("Impossibile connettersi al nodo %s: %v", neighbour, err)
				//TODO recupero da nodo non funzionante?? O quando non ricevo il segnale
			} else {
				nodeClient := pb.NewNodeServiceClient(conn)
				_, err = nodeClient.HeartBeat(context.Background(), &pb.HeartBeatMessage{Nodes: heartbeat, FromWho: n.address})
				if err != nil {
					log.Printf("heartbeat andato male con nodo %s: %v", neighbour, err)
				}
			}

		}
		time.Sleep(120 * time.Second)
	}

}
func (n *NodeServer) startGrpcServer() {
	// Avvio del server gRPC
	listener, err := net.Listen("tcp", n.address)
	if err != nil {
		log.Fatalf("Errore nel creare il listener: %v", err)
	}

	grpcServer := grpc.NewServer()
	pb.RegisterNodeServiceServer(grpcServer, n)

	log.Printf("Nodo in ascolto su %s", n.address)
	if err := grpcServer.Serve(listener); err != nil {
		log.Fatalf("Errore nell'avvio del nodo: %v", err)
	}

}
func main() {
	var coordinate Coordinates
	bootstrapAddress := "bootstrap:50051"
	myAddress := os.Getenv("MY_NODE_ADDRESS")
	// Creazione del nodo
	node := &NodeServer{
		coordinates:          coordinate,
		neighbours:           map[string]Coordinates{},
		neighboursNeighbours: map[string][]string{},
		resources:            map[string]string{},
		tempResource:         map[string]string{},
		address:              myAddress,
	}
	// Connessione al Bootstrap Server
	conn, err := grpc.Dial(bootstrapAddress, grpc.WithInsecure())
	if err != nil {
		log.Fatalf("Impossibile connettersi al bootstrap node %s: %v", bootstrapAddress, err)
	}
	defer conn.Close()

	client := pb.NewBootstrapServiceClient(conn)
	// Ottenere il nodo attivo
	var s []string //Nodi esclusi come nodo attivo
	s = append(s, myAddress)

	originAddress, err := client.FindActiveNode(context.Background(), &pb.AddressList{Ad: s}) //assunzione semplificativa bootstrap server non può fare errori
	if err != nil {
		log.Printf("Sono il primo nodo della rete!")
		node.coordinates = Coordinates{MinX: 0, MaxX: 1, MinY: 0, MaxY: 1, CenterX: 0.5, CenterY: 0.5}
	} else {
		// Nuovo nodo che si unisce alla rete
		x := rand.Float32()
		y := rand.Float32()
		point := pb.Point{X: x, Y: y}
		for { //se non sei il primo della lista provi finché non trovi un nodo funzionante, se non c'è sei il primo
			conn2, err2 := grpc.Dial(originAddress.GetAddress(), grpc.WithInsecure())
			if err2 != nil {
				log.Printf("Impossibile connettersi al nodo %s: %v", originAddress.GetAddress(), err2)
				log.Printf("Provo a chiedere un altro nodo")
				s = append(s, originAddress.GetAddress())
				originAddress, err2 = client.FindActiveNode(context.Background(), &pb.AddressList{Ad: s})
				if err2 != nil {
					log.Printf("Sono il primo nodo della rete, gli altri non funzionano!")
					node.coordinates = Coordinates{MinX: 0, MaxX: 1, MinY: 0, MaxY: 1, CenterX: 0.5, CenterY: 0.5}
					break
				}
			} else {
				log.Printf("mi sono conesso al nodo %s", originAddress.GetAddress())
				newNodeInfo := &pb.NewNodeInformation{Point: &point, Address: &pb.IPAddress{Address: myAddress}}
				nodeClient := pb.NewNodeServiceClient(conn2)
				info, err3 := nodeClient.AddNode(context.Background(), newNodeInfo)
				if err3 != nil {
					log.Fatalf("Errore nell'aggiunta del nodo da parte di %s, a causa: %v", originAddress.GetAddress(), err3)
				}
				neighboursNeighbours := make([]string, 0, len(info.NeighboursOldNode))
				for _, neighbour := range info.NeighboursOldNode {
					n := convertNodeInformationFromMessage(neighbour)
					neighboursNeighbours = append(neighboursNeighbours, n.Address)
					node.neighbours[n.Address] = n.Coordinates
				}
				for _, resource := range info.Resources {
					node.resources[resource.Key] = resource.Value
				}
				node.coordinates = convertCoordinateFromMessage(info.NewCoordinate)
				node.neighbours[info.OldNode.OriginNode.GetAddress()] = convertCoordinateFromMessage(info.OldNode.Coordinate)
				node.neighboursNeighbours[info.OldNode.OriginNode.GetAddress()] = neighboursNeighbours
				break
			}
		}
	}
	_, err = client.AddNode(context.Background(), &pb.IPAddress{Address: myAddress})
	if err != nil {
		log.Fatalf("Errore nell'aggiungersi alla rete")
	}
	log.Printf("Le mie coordinate sono x_min: %f, x_max: %f, y_min: %f, y_max: %f", node.coordinates.MinX, node.coordinates.MaxX, node.coordinates.MinY, node.coordinates.MaxY)
	node.removeOldNeighbours()
	log.Printf("my neighbours are %v", node.neighbours)

	//Avvio procedura heartbeat
	go node.sendHeartBeatAllNeighbours()
	go node.startGrpcServer()
	reader := bufio.NewReader(os.Stdin)
	for {
		fmt.Println("1) Input resource \n2) Get resource \n3) Delete resource \n4) Print all resources")
		fmt.Print("Enter choice: ")
		choice, errScan := reader.ReadString('\n')
		if errScan != nil {
			log.Printf("Errore input: %v", errScan)
			continue
		}
		choice = strings.TrimSpace(choice)

		switch choice {
		case "1":
			fmt.Print("Enter name: ")
			key, _ := reader.ReadString('\n')
			key = strings.TrimSpace(key)

			fmt.Print("Enter content: ")
			value, _ := reader.ReadString('\n')
			value = strings.TrimSpace(value)

			toWho, err := node.AddResource(context.Background(), &pb.Resource{Key: key, Value: value})
			if err != nil {
				fmt.Printf("Errore nell'aggiunta della risorsa: %v", err)

			} else {
				fmt.Printf("risorsa aggiunta al nodo %s\n", toWho.GetAddress())
			}

		case "2":
			fmt.Print("Enter name: ")
			key, _ := reader.ReadString('\n')
			key = strings.TrimSpace(key)

			r, err := node.GetResource(context.Background(), &pb.Key{K: key})
			if err != nil {
				fmt.Printf("Errore nel recupero della risorsa, %v\n", err)
			} else {
				fmt.Printf("%s\n", r.Value)
			}

		case "3":
			fmt.Print("Enter name: ")
			key, _ := reader.ReadString('\n')
			key = strings.TrimSpace(key)

			_, err := node.DeleteResource(context.Background(), &pb.Key{K: key})
			if err != nil {
				fmt.Printf("Errore nel eliminazione della risorsa, %v\n", err)
			}

		case "4":
			for n, value := range node.resources {
				fmt.Printf("%s: %s\n", n, value)
			}

		default:
			fmt.Println("Errore, scelta non valida.")
		}
	}

}
