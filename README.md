# Solana Balance API

### Features

- **Concurrent Balance Fetching:** Fetches balances for multiple wallets in parallel for maximum speed.
- **IP Rate Limiting:** Limits clients to 10 requests per minute to prevent abuse.
- **Caching with 10s TTL:** Caches results for wallet addresses to reduce RPC calls and improve response times.
- **Mutex on Concurrent Requests:** Prevents duplicate RPC calls ("thundering herd") if multiple requests for the same uncached wallet arrive simultaneously.
- **MongoDB Authentication:** Secures the endpoint with API keys stored in a MongoDB database.
- **Production-Ready Docker Image:** Built using a multi-stage, non-root, minimal (`alpine`) Docker image for security and small size.
- **Automated CI/CD:** A GitHub Actions workflow automatically builds and pushes the Docker image to Docker Hub on every push to the `main` branch.

### Prerequisites

- [Docker](https://www.docker.com/products/docker-desktop/) must be installed and running on your machine.

---

### How to Run the Application

#### Step 1: Create a Docker Network

This provides a private virtual network for our containers to communicate securely by name.

```bash
docker network create solana-network
```

#### Step 2: Start the MongoDB Database

```bash
docker run -d \
  --network solana-network \
  --name solana-mongo \
  -p 27017:27017 \
  mongo
```

#### Step 3: Insert a Valid API Key

We will use `my-secret-api-key` as our example key.

```bash
# 1. Connect to the database shell inside the container
docker exec -it solana-mongo mongosh

# 2. Inside the mongosh shell, run these commands to insert the key
use api_auth
db.api_keys.insertOne({ "key": "my-secret-api-key" })
exit
```

#### Step 4: Run the API Container

```bash
docker run -d \
  --network solana-network \
  --name solana-api \
  -p 8080:8080 \
  -e MONGO_URI="mongodb://solana-mongo:27017" \
  boltcola/solana-balance-api:latest
```

**The API is now running and accessible at `http://localhost:8080`.**

---

### How to Test the API

You can use `curl` or any API client to test the running endpoint.

#### Test 1: Successful Request (Single Wallet)

This is the standard "happy path" test.

```bash
curl -X POST http://localhost:8080/api/get-balance \
-H "Content-Type: application/json" \
-H "X-API-Key: my-secret-api-key" \
-d '{"wallets": ["7xLk17EQQ5KLDLDe44wCmupJKJjTGd8hs3eSVVhCx932"]}'
```

#### Test 2: Authentication Failure

This shows the `authMiddleware` blocking a request with an invalid key.

```bash
curl -X POST http://localhost:8080/api/get-balance \
-H "Content-Type: application/json" \
-H "X-API-Key: some-wrong-key" \
-d '{"wallets": ["7xLk17EQQ5KLDLDe44wCmupJKJjTGd8hs3eSVVhCx932"]}'
```

_Expected Response:_ `Invalid API Key`

#### Test 3: Rate Limiting

This loop will send 10 rapid requests. The first 5 will succeed, and the rest will be blocked, proving the rate limiter is working.

```bash
for i in {1..10}; do
  curl --silent -X POST http://localhost:8080/api/get-balance \
  -H "Content-Type: application/json" \
  -H "X-API-Key: my-secret-api-key" \
  -d "{\"wallets\": [\"GeJbA8a4e8G4gZass5xG4f2G38p2G2G38p2G$(date +%s%N)\"]}" # Unique wallet to bypass cache
  echo ""
done
```

_Expected Output:_ The first 5 requests will return a JSON error for an invalid address, and the next 5 will return `Too Many Requests`.

---
