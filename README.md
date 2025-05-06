# Distributed Hash Table (DHT) System

Sistema distribuito basato su una DHT, con supporto per deployment locale via Docker Compose e su cloud AWS EC2.

---

## Deployment con Docker Compose

1. **Avviare il sistema:**

   ```bash
   docker compose up -d

    Connettersi a un nodo specifico (es. nodo i):

docker attach dhtsystem-node_i-1

Interfaccia interattiva del nodo:

Inserire il numero corrispondente all’operazione desiderata:

    1) Inserisci risorsa
    2) Ottieni risorsa
    3) Elimina risorsa
    4) Stampa tutte le risorse
    5) Rimuovi nodo

Deployment su AWS EC2

    Eseguire lo script di provisioning:

python aws.py

Una volta connessi alla macchina EC2, utilizzare i seguenti comandi curl:

    Aggiungere una risorsa:

curl -X POST "http://localhost:8080/addResource?key=key1&value=value1"

Ottenere una risorsa:

curl "http://localhost:8080/getResource?key=key1"

Eliminare una risorsa:

curl -X DELETE "http://localhost:8080/deleteResource?key=key1"

Rimuovere un nodo:

        curl -X POST "http://localhost:8080/removeNode"

Cleanup (Distruzione delle risorse)

Per distruggere l'infrastruttura creata tramite Terraform:

terraform destroy
  
