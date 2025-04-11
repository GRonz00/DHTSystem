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
	"strconv"
	"strings"
	"time"
)

const NREP = 2

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
	backupNodes          []string               //Nodi che si occupano del backup delle mie risorse (scelti casualmente tra i vicini,
	// cosi so se sono ancora attivi ecc.) Il numero è dato dal min(N rep, #vicini)
	backupNodesNeighbour map[string][]string          //In caso di failure il vicino che deve recuperare deve sapere chi detiene le risorse
	nodesIBackup         map[string]map[string]string //Backup risorse altri nodi (Cosi conosco sia i nodi a cui faccio da backup che le loro risorse)
}

func (n *NodeServer) contain(point Point) bool {
	return point.X >= n.coordinates.MinX && point.X <= n.coordinates.MaxX &&
		point.Y >= n.coordinates.MinY && point.Y <= n.coordinates.MaxY
}

func stringToPoint(s string) Point {
	hash := sha256.Sum256([]byte(s))

	// Usa i primi 8 byte per X
	numX := binary.BigEndian.Uint64(hash[:8])
	// Usa i successivi 8 byte per Y
	numY := binary.BigEndian.Uint64(hash[8:16])

	// Converte in float32 tra 0 e 1
	x := float32(numX) / float32(^uint64(0))
	y := float32(numY) / float32(^uint64(0))

	return Point{X: x, Y: y}
}
func (n *NodeServer) AddResourceToBackupNode(ctx context.Context, resource *pb.AddBackup) (*pb.Bool, error) {
	n.nodesIBackup[resource.Address][resource.Resource.Key] = resource.Resource.Value
	return nil, nil
}
func (n *NodeServer) DeleteResourceToBackupNode(ctx context.Context, resource *pb.DeleteBackup) (*pb.Bool, error) {
	_, ok := n.nodesIBackup[resource.Address][resource.Key]
	if ok == true {
		delete(n.nodesIBackup[resource.Address], resource.Key)
	}
	return nil, nil
}
func (n *NodeServer) AddResource(ctx context.Context, resource *pb.Resource) (*pb.IPAddress, error) {
	point := stringToPoint(resource.Key)
	if n.contain(point) {
		n.resources[resource.Key] = resource.Value
		log.Printf("Added resource %s to node %s", resource.Key, n.address)
		for _, backupNode := range n.backupNodes {
			backupClient, _ := createClient(backupNode)
			backupClient.AddResourceToBackupNode(context.Background(), &pb.AddBackup{Address: n.address, Resource: resource})
		}
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
		nodeClient := pb.NewNodeServiceClient(conn)
		return nodeClient.AddResource(context.Background(), resource)
	}
}
func (n *NodeServer) DeleteResource(ctx context.Context, resourceKey *pb.Key) (*pb.IPAddress, error) {
	point := stringToPoint(resourceKey.K)
	if n.contain(point) {
		delete(n.resources, resourceKey.K)
		for _, backupNode := range n.backupNodes {
			backupClient, _ := createClient(backupNode)
			backupClient.DeleteResourceToBackupNode(context.Background(), &pb.DeleteBackup{Address: n.address, Key: resourceKey.K})
		}
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
				for _, backup := range n.backupNodes {
					if neighbour == backup { //Quello che mi faceva da backup, non è più un mio vicino lo cambio

					}
				}
				delete(n.neighbours, neighbour)
				delete(n.neighboursNeighbours, neighbour)
			}
			log.Printf("Ho rimosso il nodo %s come vicino", neighbour)
		}
	}
}
func (n *NodeServer) DeleteResourcesBackup(ctx context.Context, resources *pb.ResourcesBackup) (*pb.Bool, error) {
	for _, resource := range resources.Resources {
		if _, ok := n.nodesIBackup[resources.Address][resource.Key]; ok {
			delete(n.nodesIBackup, resource.Key)
		}
	}
	return nil, nil
}
func (n *NodeServer) AddResourcesBackup(ctx context.Context, resources *pb.ResourcesBackup) (*pb.Bool, error) {
	for _, resource := range resources.Resources {
		n.nodesIBackup[resources.Address][resource.Key] = resource.Value
	}
	return nil, nil
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
			//risorse di ci si deve occupare il nuovo nodo
			resourceOfNeighbour = append(resourceOfNeighbour, &pb.Resource{Key: key, Value: value})
			delete(n.resources, key)
		}
	}
	//Devo tenere aggiornate le copie di backup
	for _, backup := range n.backupNodes {
		backupClient, _ := createClient(backup)
		backupClient.AddResourcesBackup(context.Background(), &pb.ResourcesBackup{Address: n.address, Resources: resourceOfNeighbour})
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

	//Devo tenere aggiornate le copie di backup

	originNode := &pb.NodeInformation{
		Coordinate: convertCoordinateToMessage(n.coordinates),
		OriginNode: &pb.IPAddress{Address: n.address},
	}
	n.sendHeartBeatAllNeighbours()
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
		nodeClient := pb.NewNodeServiceClient(conn)
		return nodeClient.AddNode(context.Background(), newNode)
	}

}
func (n *NodeServer) UnionZone(ctx context.Context, info *pb.InformationToAddNode) (*pb.Bool, error) {
	for _, r := range info.Resources {
		n.resources[r.Key] = r.Value
	}
	//Aggiorni i nodi di backup
	for _, backup := range n.backupNodes {
		backupClient, _ := createClient(backup)
		backupClient.AddResourcesBackup(context.Background(), &pb.ResourcesBackup{Resources: info.Resources, Address: n.address})
	}

	cooOldNode := convertCoordinateFromMessage(info.NewCoordinate)

	// Calcolo nuova zona come bounding box
	n.coordinates.MinX = float32(math.Min(float64(n.coordinates.MinX), float64(cooOldNode.MinX)))
	n.coordinates.MinY = float32(math.Min(float64(n.coordinates.MinY), float64(cooOldNode.MinY)))
	n.coordinates.MaxX = float32(math.Max(float64(n.coordinates.MaxX), float64(cooOldNode.MaxX)))
	n.coordinates.MaxY = float32(math.Max(float64(n.coordinates.MaxY), float64(cooOldNode.MaxY)))
	log.Printf("Le mie coordinate a seguito del unione sono x_min: %f, x_max: %f, y_min: %f, y_max: %f", n.coordinates.MinX, n.coordinates.MaxX, n.coordinates.MinY, n.coordinates.MaxY)

	for _, neighbour := range info.NeighboursOldNode {
		n.neighbours[neighbour.OriginNode.Address] = convertCoordinateFromMessage(neighbour.Coordinate)
	}
	for neig, _ := range n.neighbours {
		client, err := createClient(neig)
		if err != nil {
			log.Printf("Errore collegamento al nodo %s: %v", neig, err)
		} else {
			_, err = client.RemoveNodeAsNeighbour(context.Background(), info.OldNode.OriginNode)
			if err != nil {
				log.Printf("Errore rimuovere vicino nodo %s: %v", neig, err)
			}

		}

	}
	n.sendHeartBeatAllNeighbours()

	return &pb.Bool{B: true}, nil
}
func (n *NodeServer) RemoveNodeAsNeighbour(ctx context.Context, neighbour *pb.IPAddress) (*pb.Bool, error) {
	_, ok := n.neighbours[neighbour.Address]
	if ok {
		delete(n.neighbours, neighbour.Address)
	}
	return nil, nil
}
func (n *NodeServer) DeleteAllResourcesBackup(ctx context.Context, who *pb.IPAddress) (*pb.Bool, error) {
	for k := range n.nodesIBackup[who.Address] {
		if _, ok := n.nodesIBackup[who.Address][k]; ok {
			delete(n.nodesIBackup[who.Address], k)
		}
	}
	return nil, nil
}
func (n *NodeServer) searchBrother() string {
	selfVolume := volumeCoordinate(n.coordinates)
	for neighbour, coordinate := range n.neighbours {
		isSquare := (n.coordinates.MaxX - n.coordinates.MinX) == (n.coordinates.MaxY - n.coordinates.MinY)
		isOnAxis := false
		sameVolume := volumeCoordinate(coordinate) == selfVolume

		if isSquare {
			isOnAxis = n.coordinates.MaxX >= coordinate.MaxX && n.coordinates.MinX <= coordinate.MinX
		} else {
			isOnAxis = n.coordinates.MaxY >= coordinate.MaxY && n.coordinates.MinY <= coordinate.MinY
		}

		if isOnAxis && sameVolume {
			// fratello trovato
			return neighbour
		}
	}
	return ""
}
func (n *NodeServer) changeZone(info *pb.Resources) {
	n.coordinates = convertCoordinateFromMessage(info.CoordinateOldNode)
	n.resources = map[string]string{}

	n.neighboursNeighbours = map[string][]string{}
	n.neighbours = map[string]Coordinates{}
	for _, res := range info.Resources {
		n.resources[res.Key] = res.Value
	}
	for _, nei := range info.NeighboursOldNode {
		n.neighbours[nei.OriginNode.Address] = convertCoordinateFromMessage(nei.Coordinate)
	}
	//Aggiorna nodi backup
	for _, backup := range n.backupNodes {
		backupClient, _ := createClient(backup)
		backupClient.DeleteAllResourcesBackup(context.Background(), &pb.IPAddress{Address: n.address})
		backupClient.AddResourcesBackup(context.Background(), &pb.ResourcesBackup{Resources: info.Resources, Address: n.address})
	}

	n.sendHeartBeatAllNeighbours()
}
func (n *NodeServer) EntrustResources(ctx context.Context, info *pb.Resources) (*pb.Bool, error) {
	//Fa una deepsearch fino a trovare due fratelli
	//Se ho un fratello, unisco le due zone lui si occupa delle zone unite e io mi vado ad'occupare della zona del nodo che se neva
	//Se non ho un fratello, affido al mio vicino più piccolo le risorse e se ne occupa lui
	log.Printf("Entrust resources")
	brother := n.searchBrother()
	if brother == "" {
		log.Print("Non ho un fratello, affido le risorse al mio vicino più piccolo")
		minVol := float32(math.MaxFloat32)
		var minN string
		for nei, coo := range n.neighbours {
			if volumeCoordinate(coo) < minVol {
				minN = nei
			}
		}
		client, err := createClient(minN)
		if err != nil {
			log.Printf("Non sono riuscito a connettermi al vicino più piccolo %s", minN)
			//todo quando non riesco affida a un altro vicino
			return nil, err
		}
		client.EntrustResources(context.Background(), info)
	} else {
		//Ho trovato un fratello
		client, err := createClient(brother)
		if err != nil {
			log.Printf("Non sono riuscito a connettermi a mio fratello %s", brother)
			//todo quando non riesco affida a un altro vicino
			return nil, err
		}
		//Lui si prende la mia zona
		neighboursCopy := make([]*pb.NodeInformation, 0, len(n.neighbours))
		for ne, coordinate := range n.neighbours {
			neighboursCopy = append(neighboursCopy, &pb.NodeInformation{
				Coordinate: convertCoordinateToMessage(coordinate),
				OriginNode: &pb.IPAddress{Address: ne},
			})
		}
		resourcesToSend := make([]*pb.Resource, 0)
		for key, value := range n.resources {
			resourcesToSend = append(resourcesToSend, &pb.Resource{Key: key, Value: value})
			delete(n.resources, key)
		}
		oldNode := pb.NodeInformation{OriginNode: &pb.IPAddress{Address: n.address}}

		client.UnionZone(context.Background(), &pb.InformationToAddNode{NewCoordinate: convertCoordinateToMessage(n.coordinates), Resources: resourcesToSend, NeighboursOldNode: neighboursCopy, OldNode: &oldNode})
		//Io mi prendo la zona del nodo che se ne va
		n.changeZone(info)

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
	selfVolume := volumeCoordinate(n.coordinates)
	for neighbour, coordinate := range n.neighbours {
		isSquare := (n.coordinates.MaxX - n.coordinates.MinX) == (n.coordinates.MaxY - n.coordinates.MinY)
		isOnAxis := false
		sameVolume := volumeCoordinate(coordinate) == selfVolume

		if isSquare {
			isOnAxis = n.coordinates.MaxX >= coordinate.MaxX && n.coordinates.MinX <= coordinate.MinX
		} else {
			isOnAxis = n.coordinates.MaxY >= coordinate.MaxY && n.coordinates.MinY <= coordinate.MinY
		}

		if isOnAxis && sameVolume {
			//create message to  nodeToSend
			neighboursCopy := make([]*pb.NodeInformation, 0, len(n.neighbours))
			for ne, coordinate := range n.neighbours {
				neighboursCopy = append(neighboursCopy, &pb.NodeInformation{
					Coordinate: convertCoordinateToMessage(coordinate),
					OriginNode: &pb.IPAddress{Address: ne},
				})
			}
			clientNode, err := createClient(neighbour)
			if err != nil {
				log.Printf("Impossibile connettersi al nodo %s per unire zone, %v", neighbour, err)
			} else {
				oldNode := pb.NodeInformation{OriginNode: &pb.IPAddress{Address: n.address}}
				_, err := clientNode.UnionZone(context.Background(), &pb.InformationToAddNode{NewCoordinate: convertCoordinateToMessage(n.coordinates), Resources: resourcesToSend, NeighboursOldNode: neighboursCopy, OldNode: &oldNode})
				if err != nil {
					log.Printf("Errore nell'unire le zone, %v", err)
				} else {
					bootstrapAddress := "bootstrap:50051"

					conn, err := grpc.Dial(bootstrapAddress, grpc.WithInsecure())
					if err != nil {
						log.Fatalf("Impossibile connettersi al bootstrap node %s: %v", bootstrapAddress, err)
					}

					client := pb.NewBootstrapServiceClient(conn)
					client.RemoveNode(context.Background(), &pb.IPAddress{Address: n.address})
					log.Fatalf("Me ne sono andato")
					return
				}
			}

		}
		if isOnAxis {
			volume := volumeCoordinate(coordinate)
			if volume < minVolume {
				minVolume = volume
				sendTo = neighbour
			}
		}

	}
	client, err := createClient(sendTo)
	log.Printf("Lascio le risorse a %s, ci pensera lui a torvare chi si occupa della zona", sendTo)
	if err != nil {
		log.Printf("Impossibile connettersi al nodo %s, %v", sendTo, err)
		delete(n.neighbours, sendTo)
		n.removeNode()
	} else {
		client.RemoveNodeAsNeighbour(context.Background(), &pb.IPAddress{Address: n.address})
		_, err := client.EntrustResources(context.Background(), &pb.Resources{Resources: resourcesToSend, CoordinateOldNode: convertCoordinateToMessage(n.coordinates)})
		if err != nil {
			log.Printf("Impossibile affidare risorse al nodo %s, %v", sendTo, err)
			delete(n.neighbours, sendTo)
			n.removeNode()
		}
		log.Fatalf("Me ne vado")
	}
}
func (n *NodeServer) HeartBeat(ctx context.Context, infoNeighbour *pb.HeartBeatMessage) (*pb.HeartBeatMessage, error) { //  Quando ricevi heartbeat aggiorna vicini e le loro coordinate
	log.Printf("HeartBeat")
	addressNeighboursNeighbour := make([]string, 0, len(infoNeighbour.Nodes))
	for _, neighbourMessage := range infoNeighbour.Nodes {
		neighbour := convertNodeInformationFromMessage(neighbourMessage)
		if neighbour.Address != infoNeighbour.FromWho {
			addressNeighboursNeighbour = append(addressNeighboursNeighbour, neighbour.Address)
		}
		n.neighbours[neighbour.Address] = neighbour.Coordinates
	}
	n.neighboursNeighbours[infoNeighbour.FromWho] = addressNeighboursNeighbour
	n.backupNodesNeighbour[infoNeighbour.FromWho] = infoNeighbour.BackupNodes
	n.removeOldNeighbours()
	if len(n.neighbours) >= NREP && len(n.backupNodes) < 2 {
		for k := range n.neighbours {
			present := false
			for _, x := range n.backupNodes {
				if x == k {
					present = true
				}
			}
			if !present {
				n.backupNodes = append(n.backupNodes, k)
				if len(n.backupNodes) == NREP {
					break
				}
			}

		}
	}
	log.Printf("i vicini a seguito heartbeat %v", n.neighbours)
	log.Printf("i vicini di %s sono: %v", infoNeighbour.FromWho, n.neighboursNeighbours[infoNeighbour.FromWho])

	return nil, nil
}

// Periodicamente e quando viene aggiunto come nodo, il nodo manda a tutti i suoi vicini le sue coordinate e i vicini (aggiornamento soft)
func (n *NodeServer) sendHeartBeatAllNeighbours() {

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
			_, err = nodeClient.HeartBeat(context.Background(), &pb.HeartBeatMessage{Nodes: heartbeat, FromWho: n.address, BackupNodes: n.backupNodes})
			if err != nil {
				log.Printf("heartbeat andato male con nodo %s: %v", neighbour, err)
			}
		}

	}

}
func (n *NodeServer) periodicHeartBeat() {
	for {
		n.sendHeartBeatAllNeighbours()
		time.Sleep(120 * time.Second)
	}
}
func (n *NodeServer) HeartBeatBackup(ctx context.Context, info *pb.Bool) (*pb.Bool, error) {
	//todo azzera timer
	return nil, nil
}
func (n *NodeServer) periodicHeartBeatBackup() {
	for {
		for _, backupNode := range n.backupNodes {
			client, _ := createClient(backupNode)
			client.HeartBeatBackup(context.Background(), &pb.Bool{})
		}
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
func abs(f float32) float32 {
	if f < 0 {
		return -f
	}
	return f
}

func randomString(n int) string {
	const letters = "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789"
	b := make([]byte, n)
	for i := range b {
		b[i] = letters[rand.Intn(len(letters))]
	}
	return string(b)
}

func findStringForPoint(targetX, targetY, tolerance float32) string {
	rand.Seed(time.Now().UnixNano())
	attempts := 0
	for {
		s := randomString(12)
		p := stringToPoint(s)

		if abs(p.X-targetX) <= tolerance && abs(p.Y-targetY) <= tolerance {
			fmt.Printf("Trovata dopo %d tentativi: %s => (X: %.6f, Y: %.6f)\n", attempts, s, p.X, p.Y)
			return s
		}

		attempts++
		if attempts%100000 == 0 {
			fmt.Printf("Tentativi: %d... ancora niente (ultimo X: %.6f, Y: %.6f)\n", attempts, p.X, p.Y)
		}
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
		nodesIBackup:         map[string]map[string]string{},
		backupNodes:          []string{},
		address:              myAddress,
	}
	// Connessione al Bootstrap Server
	conn, err := grpc.Dial(bootstrapAddress, grpc.WithInsecure())
	if err != nil {
		log.Fatalf("Impossibile connettersi al bootstrap node %s: %v", bootstrapAddress, err)
	}

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
	//Devo scegliere quali nodi avranno funzione di backup (random)
	for k := range node.neighbours {
		node.backupNodes = append(node.backupNodes, k)
		if len(node.backupNodes) == int(math.Min(float64(NREP), float64(len(node.neighbours)))) {
			break
		}
	}
	//Avvio procedura heartbeat
	go node.periodicHeartBeat()
	go node.periodicHeartBeatBackup()
	go node.startGrpcServer()

	reader := bufio.NewReader(os.Stdin)
	for {
		fmt.Println("1) Input resource \n2) Get resource \n3) Delete resource \n4) Print all resources\n5)Remove Node")
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
		case "5":
			node.removeNode()
		case "6":
			fmt.Print("Enter x: ")
			point, _ := reader.ReadString('\n')
			point = strings.TrimSpace(point)
			x, errF := strconv.ParseFloat(point, 32)
			if errF != nil {
				log.Printf("Errore conversione a float")
			} else {
				fmt.Print("Enter y: ")
				point, _ = reader.ReadString('\n')
				point = strings.TrimSpace(point)
				y, errF := strconv.ParseFloat(point, 32)
				if errF != nil {
					log.Printf("Errore conversione a float")
				} else {
					k := findStringForPoint(float32(x), float32(y), 0.1)
					toWho, err := node.AddResource(context.Background(), &pb.Resource{Key: k, Value: k})
					if err != nil {
						fmt.Printf("Errore nell'aggiunta della risorsa: %v", err)

					} else {
						fmt.Printf("risorsa aggiunta al nodo %s\n", toWho.GetAddress())
					}
				}

			}

		default:
			fmt.Println("Errore, scelta non valida.")
		}
	}

}
