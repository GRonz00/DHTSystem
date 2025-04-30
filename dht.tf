terraform {
  required_providers {
    aws = {
      source = "hashicorp/aws"
    }
  }
}

provider "aws" {
  profile = "default"
  region  = "us-east-1"
}

# Create AWS VPC
resource "aws_vpc" "dht-vpc" {
  cidr_block           = var.vpc_cidr
  enable_dns_support   = true
  enable_dns_hostnames = true

  tags = {
    Name = "${var.tag}-vpc"
  }
}

# Create subnet
resource "aws_subnet" "dht-subnet" {
  vpc_id                  = aws_vpc.dht-vpc.id
  cidr_block              = var.vpc_cidr
  map_public_ip_on_launch = true

  tags = {
    Name = "${var.tag}-subnet"
  }
}

# Create Internet Gateway
resource "aws_internet_gateway" "dht-ig" {
  vpc_id = aws_vpc.dht-vpc.id

  tags = {
    Name = "${var.tag}-ig"
  }
}

# Create route table
resource "aws_route_table" "dht-route-table" {
  vpc_id = aws_vpc.dht-vpc.id

  route {
    cidr_block = "0.0.0.0/0"
    gateway_id = aws_internet_gateway.dht-ig.id
  }

  tags = {
    Name = "${var.tag}-route-table"
  }
}

# Associate Route Table to Subnet
resource "aws_route_table_association" "crta-subnet" {
  subnet_id      = aws_subnet.dht-subnet.id
  route_table_id = aws_route_table.dht-route-table.id
}

# Create Security Group
resource "aws_security_group" "dht-security-group" {
  vpc_id      = aws_vpc.dht-vpc.id
  description = "DHT Security Group"

  egress {
    from_port   = 0
    to_port     = 0
    protocol    = "-1"
    cidr_blocks = ["0.0.0.0/0"]
  }

  # SSH used for configuration
  ingress {
    cidr_blocks = ["0.0.0.0/0"]
    from_port   = 22
    to_port     = 22
    protocol    = "tcp"
  }


  # 8888 used by nodes
  ingress {
    cidr_blocks = ["0.0.0.0/0"]
    from_port   = 8888
    to_port     = 8888
    protocol    = "tcp"
  }
  # server http
  ingress {
      cidr_blocks = ["0.0.0.0/0"]
      from_port   = 8080
      to_port     = 8080
      protocol    = "tcp"
    }


  tags = {
    Name = "${var.tag}-security-group"
  }
}

# Create EC2 instance for initial bootstrap
resource "aws_instance" "dht-bootstrap" {
  ami                    = var.aws_ami
  instance_type          = var.instance
  subnet_id              = aws_subnet.dht-subnet.id
  vpc_security_group_ids = [aws_security_group.dht-security-group.id]
  key_name               = var.key_pair

  tags = {
    Name = "${var.tag}-node-bootstrap"
  }
}

# Create EC2 instance for initial workers
resource "aws_instance" "dht-worker" {
  count                  = var.worker_count
  ami                    = var.aws_ami
  instance_type          = var.instance
  subnet_id              = aws_subnet.dht-subnet.id
  vpc_security_group_ids = [aws_security_group.dht-security-group.id]
  key_name               = var.key_pair

  tags = {
    Name = "${var.tag}-node-worker"
  }
}

output "dht-bootstrap-host-public" {
  value = aws_instance.dht-bootstrap.public_ip
}

output "dht-workers-hosts-public" {
  value = aws_instance.dht-worker.*.public_ip
}
