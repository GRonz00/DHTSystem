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
	"sync"
	"time"
)

const NREP = 2
const HEARTBEAT_INTERVAL = 60

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
	heartBeatTimer       map[string]*time.Timer
	neighboursNeighbours map[string][]string //Mappa con i vicini del vicino, usata in caso di failure per takeover
	resources            map[string]string   //La chiave è il nome della risorsa (La posizione è data dal hash del nome)
	backupNodes          []string            //Nodi che si occupano del backup delle mie risorse (scelti casualmente tra i vicini,
	// cosi so se sono ancora attivi ecc.) Il numero è dato dal min(N rep, #vicini)
	backupNodesNeighbour map[string][]string          //In caso di failure il vicino che deve recuperare deve sapere chi detiene le risorse
	nodesIBackup         map[string]map[string]string //Backup risorse altri nodi (Cosi conosco sia i nodi a cui faccio da backup che le loro risorse)
	backupTimer          map[string]*time.Timer
	muRes                sync.Mutex
	mu                   sync.Mutex
}

func (n *NodeServer) StartOrResetHeartBeat(id string, duration time.Duration) {
	if timer, exists := n.heartBeatTimer[id]; exists {
		if !timer.Stop() {
			<-timer.C
		}
	}

	n.heartBeatTimer[id] = time.AfterFunc(duration, func() {
		n.startTakeover(id)
		delete(n.heartBeatTimer, id)
	})
}
func (n *NodeServer) StartOrResetBackup(id string, duration time.Duration) {
	if timer, exists := n.backupTimer[id]; exists {
		if !timer.Stop() {
			<-timer.C
		}
	}

	n.backupTimer[id] = time.AfterFunc(duration, func() {
		n.changeBackup(id) // chiamata al metodo con receiver
		delete(n.backupTimer, id)
	})
}
func (n *NodeServer) changeBackup(id string) {

	bootClient, _ := createBootstrapClient("bootstrap:50051")
	backup, err := bootClient.FindActiveNode(context.Background(), &pb.AddressList{Ad: append(n.backupNodes, n.address)})
	if err != nil {
		log.Printf("Non è stato trovato un nodo disponibile per sostituire backup")
	} else {
		n.backupNodes = append(n.backupNodes, backup.Address)
		client, _ := createClient(backup.Address) //todo controllo errore
		resourceOfNeighbour := make([]*pb.Resource, 0, len(n.resources))
		for key, value := range n.resources {
			resourceOfNeighbour = append(resourceOfNeighbour, &pb.Resource{Key: key, Value: value})
		}
		client.AddResourcesBackup(context.Background(), &pb.ResourcesBackup{Resources: resourceOfNeighbour, Address: n.address})
	}
	for i, node := range n.backupNodes {
		if node == id {
			n.backupNodes = append(n.backupNodes[:i], n.backupNodes[i+1:]...)
		}
	}

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

	if _, ok := n.nodesIBackup[resource.Address]; !ok {
		n.nodesIBackup[resource.Address] = make(map[string]string)
	}
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
func convertPointFromMessage(point *pb.Point) Point {
	return Point{X: point.X, Y: point.Y}
}
func (n *NodeServer) SearchNode(ctx context.Context, info *pb.NodeResearch) (*pb.IPAddress, error) {
	p := convertPointFromMessage(info.Point)
	if n.contain(p) {
		return &pb.IPAddress{Address: n.address}, nil
	}
	minDistance := math.MaxFloat64 // Inizializza con il massimo valore possibile
	bestNode := ""
	for k, v := range n.neighbours {
		excluded := false
		for _, en := range info.ExcludeNodes {
			if en == k {
				excluded = true
				break
			}
		}
		if excluded {
			continue
		}
		nodePoint := Point{X: v.CenterX, Y: v.CenterY}
		dist := calculateDistance(nodePoint, p)
		if dist < minDistance {
			minDistance = dist
			bestNode = k
		}
	}
	if bestNode == "" {
		return nil, errors.New("non avevo vicini per cercare il nodo")
	}
	client, err := createClient(bestNode)
	if err != nil {
		return nil, err
	}
	info.ExcludeNodes = append(info.ExcludeNodes, n.address)
	node, err := client.SearchNode(context.Background(), info)
	if err != nil {
		return nil, err
	}
	return &pb.IPAddress{Address: node.Address}, nil
}
func (n *NodeServer) flooding(invalidNode string, key string) pb.NodeServiceClient {
	for nei := range n.neighbours {
		if nei != invalidNode {
			client, err := createClient(nei)
			if err != nil {
				continue
			}
			p := stringToPoint(key)
			log.Printf("flooding verso %s", nei)
			node, err := client.SearchNode(context.Background(), &pb.NodeResearch{Point: &pb.Point{X: p.X, Y: p.Y}, ExcludeNodes: []string{n.address, invalidNode}})
			if err != nil {
				log.Printf("Error searching node: %v", err)
				continue
			}
			client, err = createClient(node.Address)
			if err != nil {
				log.Printf("Error creating client with %s: %v", node.Address, err)
				continue
			}
			return client
		}
	}
	return nil
}
func (n *NodeServer) AddResource(ctx context.Context, resource *pb.Resource) (*pb.IPAddress, error) {
	n.muRes.Lock()
	defer n.muRes.Unlock()
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
		conn, err := grpc.Dial(nodeToRouting, grpc.WithInsecure(), grpc.WithBlock(), grpc.WithTimeout(3*time.Second))
		if err != nil {
			log.Printf("Impossibile connettersi al nodo %s: %v", nodeToRouting, err)
			client := n.flooding(nodeToRouting, resource.Key)
			if client != nil {
				return client.AddResource(context.Background(), resource)
			}

			return nil, errors.New("anche con flooding aggiunta risorsa andata male")
		}
		nodeClient := pb.NewNodeServiceClient(conn)
		return nodeClient.AddResource(context.Background(), resource)
	}
}
func (n *NodeServer) DeleteResource(ctx context.Context, resourceKey *pb.Key) (*pb.IPAddress, error) {
	n.muRes.Lock()
	defer n.muRes.Unlock()
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
		conn, err := grpc.Dial(nodeToRouting, grpc.WithInsecure(), grpc.WithBlock(), grpc.WithTimeout(3*time.Second))
		if err != nil {
			log.Printf("Impossibile connettersi al nodo %s: %v", nodeToRouting, err)
			client := n.flooding(nodeToRouting, resourceKey.K)
			if client != nil {
				return client.DeleteResource(context.Background(), resourceKey)
			}

			return nil, errors.New("anche con flooding eliminazione risorsa andata male")
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
		conn, err := grpc.Dial(nodeToRouting, grpc.WithInsecure(), grpc.WithBlock(), grpc.WithTimeout(3*time.Second))
		if err != nil {
			log.Printf("Impossibile connettersi al nodo %s: %v", nodeToRouting, err)
			client := n.flooding(nodeToRouting, resourceKey.K)
			if client != nil {
				return client.GetResource(context.Background(), resourceKey)
			}

			return nil, errors.New("anche con flooding ricerca risorsa andata male")
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
		// Verifica contatto orizzontale
		isHorizontallyAdjacent := (n.coordinates.MaxX == coordinates.MinX || n.coordinates.MinX == coordinates.MaxX) &&
			// La sovrapposizione deve essere sull'asse Y (non solo un angolo)
			(n.coordinates.MinY < coordinates.MaxY && n.coordinates.MaxY > coordinates.MinY)

		// Verifica contatto verticale
		isVerticallyAdjacent := (n.coordinates.MaxY == coordinates.MinY || n.coordinates.MinY == coordinates.MaxY) &&
			// La sovrapposizione deve essere sull'asse X (non solo un angolo)
			(n.coordinates.MinX < coordinates.MaxX && n.coordinates.MaxX > coordinates.MinX)

		// Se i nodi sono effettivamente vicini lungo un lato, o se è lo stesso nodo, lo lasciamo
		if !(isHorizontallyAdjacent || isVerticallyAdjacent) || neighbour == n.address {
			// Rimuovi il nodo dai vicini se non si toccano realmente lungo un lato
			delete(n.neighbours, neighbour)
			delete(n.neighboursNeighbours, neighbour)

			// Ferma eventuali heartbeat attivi
			if timer, ok := n.heartBeatTimer[neighbour]; ok {
				timer.Stop()
				delete(n.heartBeatTimer, neighbour)
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

	if _, ok := n.nodesIBackup[resources.Address]; !ok {
		n.nodesIBackup[resources.Address] = make(map[string]string)
	}
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
		backupClient.DeleteResourcesBackup(context.Background(), &pb.ResourcesBackup{Address: n.address, Resources: resourceOfNeighbour})
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
	n.StartOrResetHeartBeat(newNode.Address.GetAddress(), time.Second*(HEARTBEAT_INTERVAL*2+20))
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
	n.mu.Lock()
	locked := true
	defer func() {
		if locked {
			n.mu.Unlock()
		}
	}()

	log.Printf("Richiesta di aggiunta nodo: %s nel punto (%f, %f)", newNode.Address.Address, newNode.Point.X, newNode.Point.Y)
	point := Point{newNode.Point.X, newNode.Point.Y}
	if n.contain(point) {
		log.Printf("È di mia competenza")
		// Per maggiore uniformità il nuovo nodo divide la zona con il vicino con la zona più grande
		maxZone := float32(0)
		var maxNeighbor string
		for neig, coordinate := range n.neighbours {
			if volumeCoordinate(coordinate) > maxZone {
				maxZone = volumeCoordinate(coordinate)
				maxNeighbor = neig
			}
		}
		if volumeCoordinate(n.coordinates) >= maxZone {
			return n.SplitZone(context.Background(), newNode)
		} else {
			log.Printf("Il vicino %s ha una zona più grande, faccio dividere a lui la zona", maxNeighbor)
			conn, err := grpc.Dial(maxNeighbor, grpc.WithInsecure())
			if err != nil {
				log.Printf("Non riesco a connettermi al nodo con zona più grande, %v", err)
				return n.SplitZone(context.Background(), newNode)
			}
			client := pb.NewNodeServiceClient(conn)
			return client.SplitZone(context.Background(), newNode)
		}
	} else {
		log.Printf("Non è di mia competenza")
		n.mu.Unlock()
		locked = false

		nodeToRouting, err := n.routing(point)
		if err != nil {
			log.Println("Errore: nessun nodo trovato per il routing")
			return nil, err
		}
		log.Printf("Lo mando al nodo %s", nodeToRouting)

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
	n.muRes.Lock()
	defer n.muRes.Unlock()

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
		if n.address != neighbour.OriginNode.Address && neighbour.OriginNode.Address != info.OldNode.OriginNode.Address {
			n.neighbours[neighbour.OriginNode.Address] = convertCoordinateFromMessage(neighbour.Coordinate)
		}
	}
	n.removeOldNeighbours()
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

	delete(n.neighbours, neighbour.Address)
	delete(n.neighboursNeighbours, neighbour.Address)
	if timer, ok := n.heartBeatTimer[neighbour.Address]; ok {
		timer.Stop()
		delete(n.heartBeatTimer, neighbour.Address)
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

func IsBinaryDivision(a, b float64) bool { // Controlla se un intervallo [a, b] è valido (nato da divisioni binarie da [0,1])

	const epsilon = 1e-9
	if b <= a {
		return false
	}

	length := b - a
	// Calcola quante volte possiamo moltiplicare length per 2 prima di arrivare a 1
	ratio := 1.0 / length
	power := math.Log2(ratio)

	// Deve essere una potenza intera di 2 (es. 1/2, 1/4, 1/8, ...)
	if !isAlmostInteger(power) {
		return false
	}

	// Verifica che a sia multiplo di length (cioè che sia allineato nella griglia)
	multiple := a / length
	if !isAlmostInteger(multiple) {
		return false
	}

	return true
}

// Verifica se un numero è quasi un intero (tolleranza su float)
func isAlmostInteger(x float64) bool {
	_, frac := math.Modf(x)
	return math.Abs(frac) < 1e-9 || math.Abs(frac-1.0) < 1e-9
}
func (n *NodeServer) searchBrother() string {

	selfVolume := volumeCoordinate(n.coordinates)
	for neighbour, coordinate := range n.neighbours {
		isSquare := (n.coordinates.MaxX - n.coordinates.MinX) == (n.coordinates.MaxY - n.coordinates.MinY)
		isOnAxis := false
		unionGenerateValidZone := false
		sameVolume := volumeCoordinate(coordinate) == selfVolume
		if !sameVolume {
			continue
		}
		unionMinX := min(n.coordinates.MinX, coordinate.MinX)
		unionMaxX := max(n.coordinates.MaxX, coordinate.MaxX)
		unionMinY := min(n.coordinates.MinY, coordinate.MinY)
		unionMaxY := max(n.coordinates.MaxY, coordinate.MaxY)
		if isSquare {
			isOnAxis = n.coordinates.MaxX == coordinate.MaxX && n.coordinates.MinX == coordinate.MinX
			unionGenerateValidZone = IsBinaryDivision(float64(unionMinY), float64(unionMaxY))
		} else {
			isOnAxis = n.coordinates.MaxY == coordinate.MaxY && n.coordinates.MinY == coordinate.MinY
			unionGenerateValidZone = IsBinaryDivision(float64(unionMinX), float64(unionMaxX))
		}
		if !isOnAxis || !unionGenerateValidZone {
			continue
		}
		return neighbour

	}
	return ""
}
func (n *NodeServer) changeZone(info *pb.Resources) {
	log.Printf("Cambio zona")
	n.muRes.Lock()
	defer n.muRes.Unlock()
	for nei := range n.neighbours {
		c, _ := createClient(nei)
		c.RemoveNodeAsNeighbour(context.Background(), &pb.IPAddress{Address: n.address})
	}
	n.coordinates = convertCoordinateFromMessage(info.CoordinateOldNode)
	n.resources = map[string]string{}
	for _, timer := range n.heartBeatTimer {
		timer.Stop()
	}
	n.heartBeatTimer = map[string]*time.Timer{}

	n.neighboursNeighbours = map[string][]string{}
	n.neighbours = map[string]Coordinates{}
	for _, res := range info.Resources {
		n.resources[res.Key] = res.Value
	}
	for _, nei := range info.NeighboursOldNode {
		if nei.OriginNode.Address != n.address && nei.OriginNode.Address != info.AddressOldNode.Address {
			n.neighbours[nei.OriginNode.Address] = convertCoordinateFromMessage(nei.Coordinate)
			client, _ := createClient(nei.OriginNode.Address)
			client.RemoveNodeAsNeighbour(context.Background(), info.AddressOldNode)
		}
	}
	n.removeOldNeighbours()
	log.Printf("I miei vicini dopo il cambio zona sono %v", n.neighbours)
	//Aggiorna nodi backup
	for _, backup := range n.backupNodes {
		backupClient, _ := createClient(backup)
		backupClient.DeleteAllResourcesBackup(context.Background(), &pb.IPAddress{Address: n.address})
		backupClient.AddResourcesBackup(context.Background(), &pb.ResourcesBackup{Resources: info.Resources, Address: n.address})
	}
	n.sendHeartBeatAllNeighbours()
}
func (n *NodeServer) EntrustResources(ctx context.Context, info *pb.Resources) (*pb.Bool, error) {
	n.mu.Lock()
	defer n.mu.Unlock()
	//Fa una deepsearch fino a trovare due fratelli
	//Se ho un fratello, unisco le due zone lui si occupa delle zone unite e io mi vado ad'occupare della zona del nodo che se neva
	//Se non ho un fratello, affido al mio vicino più piccolo le risorse e se ne occupa lui
	log.Printf("Entrust resources da %s", info.WhoSend)
	brother := n.searchBrother()
	if brother == "" {
		log.Print("Non ho un fratello, affido le risorse al mio vicino più piccolo")
		minVol := float32(math.MaxFloat32)
		var minN string
		for nei, coo := range n.neighbours {
			log.Printf("Vicino che controllo: %s, %v", nei, coo)
			if info.WhoSend != nei && volumeCoordinate(coo) < minVol {
				minN = nei
				minVol = volumeCoordinate(coo)
			}
		}
		client, err := createClient(minN)
		if err != nil {
			log.Printf("Non sono riuscito a connettermi al vicino più piccolo %s", minN)

			//todo quando non riesco affida a un altro vicino
			return nil, err
		}
		log.Printf("Il vicino più piccolo è %s", minN)
		info.WhoSend = n.address
		go client.EntrustResources(context.Background(), info)
	} else {
		log.Printf("Ho un fratello a cui lasciare la mia zona %s", brother)
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

		_, err = client.UnionZone(context.Background(), &pb.InformationToAddNode{NewCoordinate: convertCoordinateToMessage(n.coordinates), Resources: resourcesToSend, NeighboursOldNode: neighboursCopy, OldNode: &oldNode})
		if err != nil {
			log.Printf("Errore unione con il fratello %s", brother)
		}
		// Calcolo nuova zona fratello come bounding box
		minX := float32(math.Min(float64(n.coordinates.MinX), float64((n.neighbours[brother]).MinX)))
		minY := float32(math.Min(float64(n.coordinates.MinY), float64(n.neighbours[brother].MinY)))
		maxX := float32(math.Max(float64(n.coordinates.MaxX), float64(n.neighbours[brother].MaxX)))
		maxY := float32(math.Max(float64(n.coordinates.MaxY), float64(n.neighbours[brother].MaxY)))
		info.NeighboursOldNode = append(info.NeighboursOldNode, &pb.NodeInformation{OriginNode: &pb.IPAddress{Address: brother}, Coordinate: &pb.Coordinate{MinX: minX, MaxY: maxY, MinY: minY, MaxX: maxX, CenterX: ((maxX-minX)/2 + minX), CenterY: ((maxY-minY)/2 + minY)}}) //Il fratello potrebbe diventare vicino del nodo che se ne va con l'unione, se non succede nel cambio zona verrà tolto
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
func createBootstrapClient(address string) (pb.BootstrapServiceClient, error) {
	conn, err := grpc.Dial(address, grpc.WithInsecure())
	if err != nil {
		log.Printf("Impossibile connettersi al nodo %s: %v", address, err)
		return nil, err
	}

	return pb.NewBootstrapServiceClient(conn), nil
}
func (n *NodeServer) removeNode() {
	if len(n.neighbours) == 0 {
		log.Fatalf("Non ho vicini a cui affidare le risorse")
		return
	}
	minVolume := float32(math.MaxFloat32) //se non trovo un nodo adeguato affido temporaneamente a quello con la zona più piccola
	resourcesToSend := make([]*pb.Resource, 0)
	for key, value := range n.resources {
		resourcesToSend = append(resourcesToSend, &pb.Resource{Key: key, Value: value})
		delete(n.resources, key)
	}
	neighboursCopy := make([]*pb.NodeInformation, 0, len(n.neighbours))
	for ne, coordinate2 := range n.neighbours {
		neighboursCopy = append(neighboursCopy, &pb.NodeInformation{
			Coordinate: convertCoordinateToMessage(coordinate2),
			OriginNode: &pb.IPAddress{Address: ne},
		})
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
		_, err := client.EntrustResources(context.Background(), &pb.Resources{Resources: resourcesToSend, WhoSend: n.address, AddressOldNode: &pb.IPAddress{Address: n.address}, CoordinateOldNode: convertCoordinateToMessage(n.coordinates), NeighboursOldNode: neighboursCopy})
		if err != nil {
			log.Printf("Impossibile affidare risorse al nodo %s, %v", sendTo, err)
			delete(n.neighbours, sendTo)
			n.removeNode()
		}
		log.Fatalf("Me ne vado")
	}
}
func (n *NodeServer) Takeover(ctx context.Context, tm *pb.TakeoverMessage) (*pb.Bool, error) {
	myVol := volumeCoordinate(n.coordinates)
	theirVol := volumeCoordinate(convertCoordinateFromMessage(tm.ByWho.Coordinate))

	if theirVol > myVol {
		return &pb.Bool{B: false}, nil
	} else if theirVol == myVol {
		if tm.ByWho.OriginNode.Address > n.address { //per evitare che due con la stessa zona proseguano entrambi
			delete(n.neighbours, tm.FailureNode)
			delete(n.neighboursNeighbours, tm.FailureNode)
			// Ferma eventuali heartbeat attivi
			if timer, ok := n.heartBeatTimer[tm.FailureNode]; ok {
				timer.Stop()
				delete(n.heartBeatTimer, tm.FailureNode)
			}
			return &pb.Bool{B: true, Coordinate: convertCoordinateToMessage(n.coordinates)}, nil
		} else {
			return &pb.Bool{B: false}, nil
		}
	}
	delete(n.neighbours, tm.FailureNode)
	delete(n.neighboursNeighbours, tm.FailureNode)
	// Ferma eventuali heartbeat attivi
	if timer, ok := n.heartBeatTimer[tm.FailureNode]; ok {
		timer.Stop()
		delete(n.heartBeatTimer, tm.FailureNode)
	}
	return &pb.Bool{B: true, Coordinate: convertCoordinateToMessage(n.coordinates)}, nil
}

func (n *NodeServer) startTakeover(who string) {

	log.Printf("Start takeover procedur for %s", who)
	neighboursCopy := make([]*pb.NodeInformation, 0, len(n.neighboursNeighbours[who]))
	for _, ne := range n.neighboursNeighbours[who] {
		if ne != n.address {
			client, _ := createClient(ne)
			b, err := client.Takeover(context.Background(), &pb.TakeoverMessage{FailureNode: who, ByWho: &pb.NodeInformation{Coordinate: convertCoordinateToMessage(n.coordinates), OriginNode: &pb.IPAddress{Address: n.address}}})
			if err == nil {
				if !b.B { //è più piccolo se ne occupa lui
					log.Printf("Ho perso contro %s per il takeover di %s", ne, who)
					return
				} else {
					neighboursCopy = append(neighboursCopy, &pb.NodeInformation{OriginNode: &pb.IPAddress{Address: ne}, Coordinate: b.Coordinate})
				}
			}

		}
	}
	log.Printf("Mi occupo io del takeover di %s", who)
	bootClient, _ := createBootstrapClient("bootstrap:50051")
	bootClient.RemoveNode(context.Background(), &pb.IPAddress{Address: who})
	if len(n.backupNodesNeighbour[who]) == 0 {
		log.Printf("Il vicino non aveva nodi per il backup")
		res := &pb.Resources{}
		res.CoordinateOldNode = convertCoordinateToMessage(n.neighbours[who])
		res.AddressOldNode = &pb.IPAddress{Address: who}
		res.NeighboursOldNode = neighboursCopy
		res.WhoSend = n.address
		n.EntrustResources(context.Background(), res)
		return
	}
	for _, backup := range n.backupNodesNeighbour[who] {
		backupClient, err := createClient(backup)
		if err != nil {
			continue
		}
		res, err := backupClient.GetBackupResources(context.Background(), &pb.IPAddress{Address: who})
		if err != nil {
			continue
		}
		log.Printf("Ho preso le risorse da %s", backup)
		res.CoordinateOldNode = convertCoordinateToMessage(n.neighbours[who])
		res.AddressOldNode = &pb.IPAddress{Address: who}
		delete(n.neighbours, who)
		delete(n.neighboursNeighbours, who)
		// Ferma eventuali heartbeat attivi
		if timer, ok := n.heartBeatTimer[who]; ok {
			timer.Stop()
			delete(n.heartBeatTimer, who)
		}
		res.NeighboursOldNode = neighboursCopy
		res.WhoSend = n.address
		n.EntrustResources(context.Background(), res)
		return
	}

}
func (n *NodeServer) HeartBeat(ctx context.Context, infoNeighbour *pb.HeartBeatMessage) (*pb.HeartBeatMessage, error) { //  Quando ricevi heartbeat aggiorna vicini e le loro coordinate
	n.mu.Lock()
	defer n.mu.Unlock()
	log.Printf("HeartBeat")
	log.Printf("I nodi che ho ricevuto per heartbeat %v", infoNeighbour.Nodes)
	addressNeighboursNeighbour := make([]string, 0, len(infoNeighbour.Nodes))
	for _, neighbourMessage := range infoNeighbour.Nodes {
		neighbour := convertNodeInformationFromMessage(neighbourMessage)
		if neighbour.Address != infoNeighbour.FromWho {
			addressNeighboursNeighbour = append(addressNeighboursNeighbour, neighbour.Address)
		}
		if neighbour.Address == infoNeighbour.FromWho {
			log.Printf("Ho ricevuto anche il nodo giusto")
		}
		n.neighbours[neighbour.Address] = neighbour.Coordinates
	}

	n.neighboursNeighbours[infoNeighbour.FromWho] = addressNeighboursNeighbour
	n.backupNodesNeighbour[infoNeighbour.FromWho] = infoNeighbour.BackupNodes
	n.StartOrResetHeartBeat(infoNeighbour.FromWho, time.Second*(HEARTBEAT_INTERVAL*2+20))
	n.removeOldNeighbours()
	if len(n.backupNodes) < NREP {
		log.Printf("Non avevo abbastanza nodi di backup")
		invalidNodes := n.backupNodes
		invalidNodes = append(invalidNodes, n.address)
		for i := len(n.backupNodes); i < NREP; i++ {
			bootClient, err := createBootstrapClient("bootstrap:50051")
			if err != nil {
				log.Printf("Impossibile connettersi al bootstrap")
			} else {
				backup, err := bootClient.FindActiveNode(context.Background(), &pb.AddressList{Ad: invalidNodes})
				if err == nil {
					if backup != nil {
						n.backupNodes = append(n.backupNodes, backup.Address)
						invalidNodes = append(invalidNodes, backup.Address)
					}
				}
			}

		}
		log.Printf("I nodi di backup sono:%v", n.backupNodes)
	}
	log.Printf("i vicini a seguito heartbeat %v", n.neighbours)
	log.Printf("i vicini di %s sono: %v", infoNeighbour.FromWho, n.neighboursNeighbours[infoNeighbour.FromWho])

	return nil, nil
}
func (n *NodeServer) GetBackupResources(ctx context.Context, who *pb.IPAddress) (*pb.Resources, error) {

	if _, ok := n.nodesIBackup[who.Address]; !ok {
		return &pb.Resources{}, nil
	}
	resources := make([]*pb.Resource, 0, len(n.nodesIBackup[who.Address]))
	for k, v := range n.nodesIBackup[who.Address] {
		resources = append(resources, &pb.Resource{Key: k, Value: v})
	}
	return &pb.Resources{Resources: resources}, nil
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
	log.Printf("I nodi che invio nel heartbeat %v", heartbeat)

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
		time.Sleep(HEARTBEAT_INTERVAL * time.Second)
	}
}
func (n *NodeServer) HeartBeatBackup(ctx context.Context, address *pb.IPAddress) (*pb.Bool, error) {
	n.StartOrResetBackup(address.Address, time.Second*HEARTBEAT_INTERVAL*2) //Azzeri timer
	return nil, nil
}
func (n *NodeServer) periodicHeartBeatBackup() {

	for {
		for backupNode := range n.nodesIBackup {
			client, _ := createClient(backupNode)
			client.HeartBeatBackup(context.Background(), &pb.IPAddress{Address: n.address})
		}
		time.Sleep(HEARTBEAT_INTERVAL * time.Second)

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
func (n *NodeServer) HandShake(ctx context.Context, newNode *pb.IPAddress) (*pb.HeartBeatMessage, error) {
	neigs := make([]*pb.NodeInformation, 0, len(n.neighbours))
	for neighbour, coo := range n.neighbours {
		neigs = append(neigs, &pb.NodeInformation{OriginNode: &pb.IPAddress{Address: neighbour}, Coordinate: convertCoordinateToMessage(coo)})
	}
	return &pb.HeartBeatMessage{Nodes: neigs}, nil
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
		backupNodes:          make([]string, 0, NREP),
		backupNodesNeighbour: map[string][]string{},
		address:              myAddress,
		heartBeatTimer:       map[string]*time.Timer{},
		backupTimer:          map[string]*time.Timer{},
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
				tempt := 0
				var info *pb.InformationToAddNode
				for ; tempt < 3; tempt++ {
					info, err = nodeClient.AddNode(context.Background(), newNodeInfo)
					if err != nil {
						log.Printf("Errore nell'aggiunta del nodo da parte di %s, a causa: %v", originAddress.GetAddress(), err)
						time.Sleep(time.Second)
					} else {
						break
					}
				}
				if tempt == 3 {
					log.Fatalf("Errore nell'aggiunta del nodo da parte di %s", originAddress.GetAddress())
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
				//Devo scegliere quali nodi avranno funzione di backup (random)
				invalidNodes := make([]string, 0, 1)
				invalidNodes = append(invalidNodes, myAddress)
				for i := 0; i < NREP; i++ {
					bootClient, _ := createBootstrapClient(bootstrapAddress)
					backup, err := bootClient.FindActiveNode(context.Background(), &pb.AddressList{Ad: invalidNodes})
					if err != nil {
						invalidNodes = append(invalidNodes, backup.Address)
					}
					node.backupNodes = append(node.backupNodes, backup.Address)

				}
				log.Printf("i nodi di backup sono %v", node.backupNodes)
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
	go node.periodicHeartBeat()
	go node.periodicHeartBeatBackup()
	go node.startGrpcServer()

	//Per problemi di concorrenza, due nodi vengono aggiunti contemporaneamente da due vicini, potrebbero essere vicini ma non saperlo
	for neighbour, _ := range node.neighbours {
		neighbourClient, _ := createClient(neighbour)
		nn, err := neighbourClient.HandShake(context.Background(), &pb.IPAddress{Address: myAddress})
		if err != nil {
			for _, x := range nn.Nodes {
				node.neighbours[x.OriginNode.Address] = convertCoordinateFromMessage(x.Coordinate)
			}
			node.removeOldNeighbours()
		}

	}
	log.Printf("my neighbours after handshake are %v", node.neighbours)
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
