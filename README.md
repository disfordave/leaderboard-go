# High-Performance Game Leaderboard API Server

> **Go, Redis, PostgreSQL**을 활용하여 대규모 트래픽 환경에서도 데이터 정합성과 실시간성을 보장하는 게임 리더보드 서비스입니다.

---

## 📖 Project Overview

이 프로젝트는 수백만 명의 유저가 동시에 점수를 갱신하고 랭킹을 조회하는 게임 환경을 가정하여 설계되었습니다.
단순한 캐싱 전략을 넘어, **Transactional Outbox Pattern**을 도입하여 **DB(PostgreSQL)**와 **Cache(Redis)** 간의 *Dual Write* 데이터 불일치 문제를 해결하고, **Batch Processing** 최적화를 통해 처리량을 극대화했습니다.

POST 요청은 Redis에 직접 쓰지 않고, PostgreSQL 트랜잭션을 통해 이벤트만 기록한 뒤 Worker가 비동기로 Redis를 갱신합니다.

---

## 🏗 Architecture

**Transactional Outbox Pattern**을 적용하여 API 요청 시점에는 DB에 이벤트만 기록하고(Atomic), 별도의 Worker가 비동기로 Redis에 반영합니다.

```mermaid
graph LR
    Client([Client])
    API[Go API Server]
    DB[(PostgreSQL)]
    Redis[(Redis Cache)]
    
    subgraph "Transactional Boundary"
        API -->|1. Insert Score & Event| DB
    end
    
    subgraph "Outbox Worker (Background)"
        DB -->|2. Batch Fetch (SKIP LOCKED)| API
        API -->|3. Pipeline Update| Redis
        API -->|4. Bulk Status Update| DB
    end

    Client -.->|5. Get Ranking (Real-time)| Redis
```

---

## 🚀 Key Features

* **Reliable Score Update**
  PostgreSQL 트랜잭션을 통해 점수 기록의 영속성을 보장합니다.

* **Real-time Leaderboard**
  Redis Sorted Set(ZSet)을 활용하여 O(log N) 복잡도로 랭킹을 산출합니다.

* **High Throughput Worker**

  * Batch Processing: Outbox 이벤트를 500개 단위로 묶어서 처리
  * Redis Pipelining: 네트워크 Round-Trip 최소화
  * Concurrency Control: `FOR UPDATE SKIP LOCKED`로 중복 처리 방지

* **Performance Tuned**

  * DB Connection Pool 튜닝
  * Worker interval 조정
  * Queue backlog 해소

---

## 📊 Performance Benchmark

k6를 사용하여 로컬 환경과 클라우드(GCP e2-small) 환경에서 부하 테스트를 수행했습니다.

### 1. Local Benchmark (Docker Environment)

순수 애플리케이션 처리량 검증

* Read (Get Top N): ~17,600 RPS (p95: 33ms)
* Write (Post Score): ~5,300 RPS (p95: 103ms)

### 2. Cloud Stability Test (GCP e2-small)

최소 사양 VM 환경에서 안정성 검증

* Scenario: 100 VUs 지속 부하
* Result: Error Rate 0.00%

Optimization:

* DB Connection Starvation 해결
* MaxOpenConns: 20 → 50
* Worker ticker 조정

---

## 🛠 Technical Challenges & Troubleshooting

### Issue: Outbox Queue Backlog & DB Starvation

초기 구현에서는 LIMIT 1 방식으로 Outbox를 처리하여 입력 속도가 처리 속도를 초과하면서 큐 적체가 발생했습니다.

### Optimization Steps

**1. Batch + Pipeline 도입**

* Batch fetch (500)
* Redis pipeline
* 처리량 약 20배 향상

**2. DB Connection Starvation 발생**

Worker가 DB 커넥션을 과점하여 API 요청이 타임아웃 발생

**3. Final tuning**

* DB MaxOpenConns: 20 → 50
* Worker ticker: 50ms

결과적으로:

* Outbox backlog 해소
* API error rate 0%

---

## 💻 Tech Stack

* Language: Go 1.24
* Database: PostgreSQL 16, Redis 7
* Libraries: pgx/v5, go-redis/v9
* Infra: Docker, Docker Compose, AWS (EC2, VPC), Terraform, GCP Compute Engine (leaderboard-go.disfordave.com)
* Testing: k6

---

## 🏃‍♂️ How to Run

### Clone repository

```git clone
git clone
cd leaderboard-go
```

### .env file

```
cp .env.example .env
```

### Start services

```
docker compose up --build -d
```

### Load test

Write test:

```
docker run --rm -i --network=host -e VUS=100 grafana/k6 run - < k6/post_scores.js
```

Read test:

```
docker run --rm -i --network=host -e VUS=100 grafana/k6 run - < k6/get_top.js
```

### Monitor outbox

```
watch -n 1 'docker exec -it leaderboard-go-postgres psql -U leaderboard -d leaderboard -c "select status, count(*) from outbox group by status;"'
```

---

## ☁️ Infrastructure & Deployment

AWS 환경에서의 안정적이고 재현 가능한 배포를 위해 **Terraform**을 활용하여
VPC, Subnet, Internet Gateway, Security Group 등 네트워크 계층부터 EC2 인스턴스 프로비저닝까지의 전 과정을 코드로 관리(IaC)합니다.

* **Automated Provisioning**: `terraform apply` 명령어 하나로 격리된 네트워크 환경과 서버를 즉시 구축합니다.
* **Bootstrapping**: EC2 User Data 스크립트를 활용하여, 인스턴스 부팅 시 Docker 설치 및 최신 애플리케이션 배포가 자동으로 수행됩니다.

### Deploy to AWS

```bash
cd terraform

# Initialize Terraform
terraform init

# Check execution plan
terraform plan

# Apply infrastructure changes
terraform apply
```

---

## 🔗 API Endpoints

| Method | Endpoint                             | Description        |
| ------ | ------------------------------------ | ------------------ |
| POST   | /v1/seasons/{sid}/scores             | 유저 점수 업데이트 (Async) |
| GET    | /v1/seasons/{sid}/leaderboard/top    | Top N 랭킹 조회        |
| GET    | /v1/seasons/{sid}/leaderboard/rank   | 특정 유저 랭킹 조회        |
| GET    | /v1/seasons/{sid}/leaderboard/around | 특정 유저 주변 랭킹 조회     |
| DELETE | /v1/seasons/{sid}                    | 시즌 데이터 초기화         |
