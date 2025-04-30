import threading
import subprocess
import json
import os
import time

def run_command(command):
    process = subprocess.Popen(
        command,
        shell=True,
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        text=True
    )
    if process.stdout is not None:
        print("STDOUT:")
        for line in process.stdout:
            print(line, end='')
    if process.stderr is not None:
        print("STDERR:")
        for line in process.stdout:
            print(line, end='')

    return_code = process.wait()
    if return_code != 0:
        print(f"Command {command} failed")
        exit(return_code)

def service(host, config, bootstrap_address):
    service = f"""[Unit]
Description=dht application

[Service]
Environment=BOOTSTRAP_ADDRESS={bootstrap_address}
Environment=MY_NODE_ADDRESS={host}
Environment=PORT={config['port']}
Environment=HTTP_PORT={config['http_port']}
Environment=HEARTBEAT_INTERVAL={config['heartbeat_interval']}
Environment=NREP={config['nrep']}
Environment=DOCKER={'false'}
Type=simple
WorkingDirectory=/home/ec2-user/dht
ExecStart=/home/ec2-user/dht/build/node
ExecStop=/bin/kill -TERM $MAINPID

[Install]
WantedBy=multi-user.target
    """
    service_file = open(f"dht.service_{host}", "w")
    service_file.write(service)
    service_file.close()

key_pem = input("Please enter the path to key.pem: ")

# Read configuration
json_file = open("config/config.json")
config = json.load(json_file)

tfvars_str = f"""key_pair = "{config['key_pair']}"
instance = "{config["instance"]}"
worker_count = {config["workers"]}
"""
tfvars = open("terraform.tfvars", "w")
tfvars.write(tfvars_str)
tfvars.close()

print("Creating AWS EC2 instances using Terraform")
run_command("terraform init")
run_command("terraform plan")
ok = input("Do you want to continue? [y/n] ").lower()
if ok == "n" or ok == "no":
    print("Plan was not approved")
    exit(1)
run_command("terraform apply -auto-approve")
run_command("terraform output -json > tf.json")
print("AWS instances created correctly")
print("Waiting 30 sec for instances to start")
time.sleep(30)
print("Deploying using custom scripts")
json_file = open("tf.json")
data = json.load(json_file)

public_bootstrap = data["dht-bootstrap-host-public"]["value"]

public_workers_hosts = data["dht-workers-hosts-public"]["value"]

threads: list[threading.Thread] = []

print("Deploying Bootstrap")
service(public_bootstrap, config,f"")
run_command(f"./node.sh {key_pem} {public_bootstrap} bootstrap")

for i in range(len(public_workers_hosts)):
    worker = public_workers_hosts[i]
    print(f"Deploying Worker {worker}")
    service(worker, config, bootstrap_address=f"{public_bootstrap}:{config["port"]}")
    t = threading.Thread(target=run_command, args=(f"./node.sh {key_pem} {worker} node",))
    threads.append(t)
    t.start()

# Wait for all threads to finish
for t in threads:
    t.join()

print("Removing temp files")
os.remove("tf.json")
os.remove(f"dht.service_{public_bootstrap}")
for worker in public_workers_hosts:
    os.remove(f"dht.service_{worker}")
os.remove("terraform.tfvars")
print(f"Correctly deployed application.\nBootstrap at: {public_bootstrap}:{config["port"]}")
for i in range(len(public_workers_hosts)):
    worker = public_workers_hosts[i]
    print(f"Worker {i} at: {worker}:{config["port"]}")
