#!/bin/bash

# 1. Install Docker, Docker Compose, and Git
apt-get update
apt-get install -y docker.io docker-compose-v2 git

# 2. Start Docker and add user to docker group
systemctl enable --now docker
usermod -aG docker ubuntu

# 3. Clone the application repository
git clone https://github.com/disfordave/leaderboard-go.git /home/ubuntu/app

cd /home/ubuntu/app

# 4. Create .env file for Docker Compose
cat <<EOF > .env
POSTGRES_USER=leaderboard
POSTGRES_PASSWORD=leaderboard
POSTGRES_DB=leaderboard
REDIS_ADDR=redis:6379
POSTGRES_DSN=postgres://leaderboard:leaderboard@postgres:5432/leaderboard?sslmode=disable
EOF

# 5. Build and run the application using Docker Compose
docker compose up --build -d