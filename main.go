package main

import (
	"context"
	"encoding/json"
	"log"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/rpc"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
	"golang.org/x/time/rate"
)

// --- App Configuration ---
const (
	rpcURL         = "https://pomaded-lithotomies-xfbhnqagbt-dedicated.helius-rpc.com/?api-key=37ba4475-8fa3-4491-875f-758894981943"
	cacheTTL       = 10 * time.Second
	rateLimitRPS   = 10.0 / 60.0 // 10 requests per minute
	rateLimitBurst = 5
)

// --- API Data Structures ---
type GetBalanceRequest struct {
	Wallets []string `json:"wallets"`
}

type WalletBalance struct {
	Wallet  string `json:"wallet"`
	Balance uint64 `json:"balance,omitempty"`
	Error   string `json:"error,omitempty"`
}

type APIKey struct {
	Key string `bson:"key"`
}

// --- Caching Layer ---
type CacheEntry struct {
	balance    uint64
	expiration time.Time
}

var (
	walletCache      = make(map[string]*CacheEntry)
	walletCacheMutex = &sync.Mutex{}
)

// inflightRequest tracks identical, concurrent RPC calls to prevent redundant work.
type inflightRequest struct {
	wg    sync.WaitGroup
	value uint64
	err   error
}

var (
	inflightRequests      = make(map[string]*inflightRequest)
	inflightRequestsMutex = &sync.Mutex{}
)

var (
	// ipLimiters stores a rate limiter for each unique client IP.
	ipLimiters      = make(map[string]*rate.Limiter)
	ipLimitersMutex = &sync.Mutex{}

	mongoClient     *mongo.Client
	solanaRPCClient *rpc.Client
)

// --- HTTP Middleware ---

func rateLimitMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			log.Printf("could not parse IP from: %s", r.RemoteAddr)
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			return
		}

		ipLimitersMutex.Lock()
		if _, found := ipLimiters[ip]; !found {
			ipLimiters[ip] = rate.NewLimiter(rate.Limit(rateLimitRPS), rateLimitBurst)
		}

		if !ipLimiters[ip].Allow() {
			ipLimitersMutex.Unlock()
			http.Error(w, http.StatusText(http.StatusTooManyRequests), http.StatusTooManyRequests)
			return
		}
		ipLimitersMutex.Unlock()

		next.ServeHTTP(w, r)
	})
}

// authMiddleware validates the provided API key against the database.
func authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		apiKey := r.Header.Get("X-API-Key")
		if apiKey == "" {
			http.Error(w, "API Key is required", http.StatusUnauthorized)
			return
		}

		collection := mongoClient.Database("api_auth").Collection("api_keys")
		err := collection.FindOne(context.TODO(), bson.M{"key": apiKey}).Err()
		if err != nil {
			http.Error(w, "Invalid API Key", http.StatusForbidden)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// --- API Handler ---

func getBalanceHandler(w http.ResponseWriter, r *http.Request) {
	var reqBody GetBalanceRequest
	if err := json.NewDecoder(r.Body).Decode(&reqBody); err != nil {
		http.Error(w, "Invalid request body", http.StatusBadRequest)
		return
	}

	var wg sync.WaitGroup
	resultsChan := make(chan WalletBalance, len(reqBody.Wallets))

	// Fetch all wallet balances concurrently.
	for _, wallet := range reqBody.Wallets {
		wg.Add(1)
		go fetchBalance(wallet, &wg, resultsChan)
	}

	wg.Wait()
	close(resultsChan)

	var results []WalletBalance
	for result := range resultsChan {
		results = append(results, result)
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(results); err != nil {
		log.Printf("Failed to encode response: %v", err)
	}
}

// fetchBalance retrieves a single wallet's balance, utilizing caching and handling concurrent requests.
func fetchBalance(wallet string, wg *sync.WaitGroup, resultsChan chan<- WalletBalance) {
	defer wg.Done()

	// First, check for a fresh cache entry.
	walletCacheMutex.Lock()
	if entry, found := walletCache[wallet]; found && time.Now().Before(entry.expiration) {
		walletCacheMutex.Unlock()
		resultsChan <- WalletBalance{Wallet: wallet, Balance: entry.balance}
		return
	}
	walletCacheMutex.Unlock()

	// This prevents a "thundering herd" problem on a cold cache.
	inflightRequestsMutex.Lock()
	if req, inFlight := inflightRequests[wallet]; inFlight {
		inflightRequestsMutex.Unlock()
		req.wg.Wait() // Wait for the original request to complete.

		var errStr string
		if req.err != nil {
			errStr = req.err.Error()
		}
		resultsChan <- WalletBalance{Wallet: wallet, Balance: req.value, Error: errStr}
		return
	}

	req := &inflightRequest{}
	req.wg.Add(1)
	inflightRequests[wallet] = req
	inflightRequestsMutex.Unlock()

	// Defer the cleanup to unblock any waiting goroutines.
	defer func() {
		inflightRequestsMutex.Lock()
		delete(inflightRequests, wallet)
		req.wg.Done()
		inflightRequestsMutex.Unlock()
	}()

	pubKey, err := solana.PublicKeyFromBase58(wallet)
	if err != nil {
		req.err = err
		resultsChan <- WalletBalance{Wallet: wallet, Error: "invalid wallet address format"}
		return // Exit early 
	}

	balance, rpcErr := solanaRPCClient.GetBalance(context.Background(), pubKey, rpc.CommitmentFinalized)
	req.err = rpcErr

	if rpcErr != nil {
		resultsChan <- WalletBalance{Wallet: wallet, Error: rpcErr.Error()}
	} else {
		req.value = balance.Value
		resultsChan <- WalletBalance{Wallet: wallet, Balance: balance.Value}

		// On success, cache the result.
		walletCacheMutex.Lock()
		walletCache[wallet] = &CacheEntry{
			balance:    balance.Value,
			expiration: time.Now().Add(cacheTTL),
		}
		walletCacheMutex.Unlock()
	}
}

// --- Main Application ---
func main() {
	mongoURI := os.Getenv("MONGO_URI")
	if mongoURI == "" {
		log.Fatal("MONGO_URI environment variable must be set.")
	}

	var err error
	mongoClient, err = mongo.Connect(context.TODO(), options.Client().ApplyURI(mongoURI))
	if err != nil {
		log.Fatalf("Failed to connect to MongoDB: %v", err)
	}
	if err := mongoClient.Ping(context.TODO(), nil); err != nil {
		log.Fatalf("Failed to ping MongoDB: %v", err)
	}
	log.Println("Successfully connected to MongoDB.")

	solanaRPCClient = rpc.New(rpcURL)

	mux := http.NewServeMux()
	apiHandler := authMiddleware(rateLimitMiddleware(http.HandlerFunc(getBalanceHandler)))
	mux.Handle("/api/get-balance", apiHandler)

	log.Println("Server starting on port 8080...")
	if err := http.ListenAndServe(":8080", mux); err != nil {
		log.Fatalf("Server failed to start: %v", err)
	}
}