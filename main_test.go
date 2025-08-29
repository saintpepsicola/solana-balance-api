package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	solanago "github.com/gagliardetto/solana-go"
	"github.com/gagliardetto/solana-go/rpc"
	"github.com/stretchr/testify/assert"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo/integration/mtest"
	"golang.org/x/time/rate"
)

// MockSolanaRPCClient is a mock implementation of the Solana RPC client.
type MockSolanaRPCClient struct {
	GetBalanceFunc func(ctx context.Context, account solanago.PublicKey, commitment rpc.CommitmentType) (*rpc.GetBalanceResult, error)
}

func (m *MockSolanaRPCClient) GetBalance(ctx context.Context, account solanago.PublicKey, commitment rpc.CommitmentType) (*rpc.GetBalanceResult, error) {
	if m.GetBalanceFunc != nil {
		return m.GetBalanceFunc(ctx, account, commitment)
	}
	return nil, nil // Default empty implementation
}

func TestAuthMiddleware(t *testing.T) {

	mt := mtest.New(t, mtest.NewOptions().ClientType(mtest.Mock))

	mt.Run("valid API key", func(mt *mtest.T) {
		mongoClient = mt.Client
		mt.AddMockResponses(mtest.CreateCursorResponse(1, "api_auth.api_keys", mtest.FirstBatch, bson.D{{"key", "valid-key"}}))

		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set("X-API-Key", "valid-key")
		rr := httptest.NewRecorder()

		nextHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		})

		authMiddleware(nextHandler).ServeHTTP(rr, req)
		assert.Equal(t, http.StatusOK, rr.Code)
	})

	mt.Run("invalid API key", func(mt *mtest.T) {
		mongoClient = mt.Client
		mt.AddMockResponses(mtest.CreateCursorResponse(0, "api_auth.api_keys", mtest.FirstBatch)) // No document found

		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.Header.Set("X-API-Key", "invalid-key")
		rr := httptest.NewRecorder()

		nextHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		})

		authMiddleware(nextHandler).ServeHTTP(rr, req)
		assert.Equal(t, http.StatusForbidden, rr.Code)
	})

	mt.Run("missing API key", func(mt *mtest.T) {
		mongoClient = mt.Client // Still need a client, even if not used for query
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		rr := httptest.NewRecorder()

		nextHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		})

		authMiddleware(nextHandler).ServeHTTP(rr, req)
		assert.Equal(t, http.StatusUnauthorized, rr.Code)
	})
}

func TestRateLimitMiddleware(t *testing.T) {
	// Reset clients map for a clean test run
	clientsMutex.Lock()
	clients = make(map[string]*rate.Limiter)
	clientsMutex.Unlock()

	nextHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	rateLimitedHandler := rateLimitMiddleware(nextHandler)

	// Simulate multiple requests from the same IP
	testIP := "127.0.0.1:12345"
	for i := 0; i < rateLimitBurst; i++ { // 10 requests per minute allowed
		req := httptest.NewRequest(http.MethodGet, "/", nil)
		req.RemoteAddr = testIP
		rr := httptest.NewRecorder()
		rateLimitedHandler.ServeHTTP(rr, req)
		assert.Equal(t, http.StatusOK, rr.Code, "Request %d should be OK", i+1)
	}

	// The (rateLimitBurst + 1)th request should be rate-limited
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = testIP
	rr := httptest.NewRecorder()
	rateLimitedHandler.ServeHTTP(rr, req)
	assert.Equal(t, http.StatusTooManyRequests, rr.Code, "Request exceeding burst limit should be blocked")

	// Wait for more than a minute for the rate limiter to reset
	time.Sleep(1*time.Minute + 1*time.Second)

	// Request should pass again after reset
	req = httptest.NewRequest(http.MethodGet, "/", nil)
	req.RemoteAddr = testIP
	rr = httptest.NewRecorder()
	rateLimitedHandler.ServeHTTP(rr, req)
	assert.Equal(t, http.StatusOK, rr.Code, "Request after reset should pass")
}

func TestGetBalanceHandler_Success(t *testing.T) {
	// Reset cache and in-flight requests for clean test runs
	walletCacheMutex.Lock()
	walletCache = make(map[string]*CacheEntry)
	walletCacheMutex.Unlock()

	inflightRequestsMutex.Lock()
	inflightRequests = make(map[string]*request)
	inflightRequestsMutex.Unlock()

	// Mock the Solana RPC client
	mockRPCClient := &MockSolanaRPCClient{
		GetBalanceFunc: func(ctx context.Context, account solana.PublicKey, commitment rpc.CommitmentType) (*rpc.GetBalanceResult, error) {
			switch account.String() {
			case "HXt8LibraryaXz2y222y222y222y222y222y222y222y":
				return &rpc.GetBalanceResult{Value: 1000}, nil
			case "HXt8LibrarybXz2y222y222y222y222y222y222y222y":
				return &rpc.GetBalanceResult{Value: 2000}, nil
			default:
				return &rpc.GetBalanceResult{Value: 0}, nil
			}
		},
	}
	solanaRPCClient = mockRPCClient

	tests := []struct {
		name           string
		requestBody    GetBalanceRequest
		expectedStatus int
		expectedWallets []string
		expectedBalances []uint64
	}{
		{
			name: "Single wallet",
			requestBody: GetBalanceRequest{
				Wallets: []string{"HXt8LibraryaXz2y222y222y222y222y222y222y222y"},
			},
			expectedStatus: http.StatusOK,
			expectedWallets: []string{"HXt8LibraryaXz2y222y222y222y222y222y222y222y"},
			expectedBalances: []uint64{1000},
		},
		{
			name: "Multiple wallets",
			requestBody: GetBalanceRequest{
				Wallets: []string{"HXt8LibraryaXz2y222y222y222y222y222y222y222y", "HXt8LibrarybXz2y222y222y222y222y222y222y222y"},
			},
			expectedStatus: http.StatusOK,
			expectedWallets: []string{"HXt8LibraryaXz2y222y222y222y222y222y222y222y", "HXt8LibrarybXz2y222y222y222y222y222y222y222y"},
			expectedBalances: []uint64{1000, 2000},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body, _ := json.Marshal(tt.requestBody)
			req := httptest.NewRequest(http.MethodPost, "/api/get-balance", bytes.NewBuffer(body))
			rr := httptest.NewRecorder()

			getBalanceHandler(rr, req)

			assert.Equal(t, tt.expectedStatus, rr.Code)

			var responses []WalletBalance
			err := json.NewDecoder(rr.Body).Decode(&responses)
			assert.NoError(t, err)
			assert.Len(t, responses, len(tt.expectedWallets))

			for i, expectedWallet := range tt.expectedWallets {
				found := false
				for _, res := range responses {
					if res.Wallet == expectedWallet {
						assert.Equal(t, tt.expectedBalances[i], res.Balance)
						assert.Empty(t, res.Error)
						found = true
						break
					}
				}
				assert.True(t, found, "Wallet %s not found in response", expectedWallet)
			}
		})
	}
}

func TestGetBalanceHandler_InvalidMethod(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/get-balance", getBalanceHandler)

	req := httptest.NewRequest(http.MethodGet, "/api/get-balance", nil)
	rr := httptest.NewRecorder()

	mux.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusMethodNotAllowed, rr.Code)
}

func TestGetBalanceHandler_InvalidBody(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/get-balance", getBalanceHandler)

	req := httptest.NewRequest(http.MethodPost, "/api/get-balance", bytes.NewBufferString("invalid json"))
	rr := httptest.NewRecorder()

	mux.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusBadRequest, rr.Code)
}

func TestGetBalanceHandler_EmptyWallets(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/get-balance", getBalanceHandler)

	body, _ := json.Marshal(GetBalanceRequest{Wallets: []string{}})
	req := httptest.NewRequest(http.MethodPost, "/api/get-balance", bytes.NewBuffer(body))
	rr := httptest.NewRecorder()

	mux.ServeHTTP(rr, req)

	assert.Equal(t, http.StatusBadRequest, rr.Code)
}

func TestGetBalanceHandler_Caching(t *testing.T) {
	// Reset cache and in-flight requests for clean test runs
	walletCacheMutex.Lock()
	walletCache = make(map[string]*CacheEntry)
	walletCacheMutex.Unlock()

	inflightRequestsMutex.Lock()
	inflightRequests = make(map[string]*request)
	inflightRequestsMutex.Unlock()

	callCount := 0
	mockRPCClient := &MockSolanaRPCClient{
		GetBalanceFunc: func(ctx context.Context, account solana.PublicKey, commitment rpc.CommitmentType) (*rpc.GetBalanceResult, error) {
			callCount++
			return &rpc.GetBalanceResult{Value: uint64(callCount)}, nil // Return a unique value to track calls
		},
	}
	solanaRPCClient = mockRPCClient

	testWallet := "HXt8LibraryaXz2y222y222y222y222y222y222y222y"
	reqBody, _ := json.Marshal(GetBalanceRequest{Wallets: []string{testWallet}})

	// First request: should fetch and cache
	req := httptest.NewRequest(http.MethodPost, "/api/get-balance", bytes.NewBuffer(reqBody))
	rr := httptest.NewRecorder()
	getBalanceHandler(rr, req)
	assert.Equal(t, http.StatusOK, rr.Code)
	var res1 []WalletBalance
	json.NewDecoder(rr.Body).Decode(&res1)
	assert.Equal(t, uint64(1), res1[0].Balance)
	assert.Equal(t, 1, callCount)

	// Second request (within TTL): should use cache
	req = httptest.NewRequest(http.MethodPost, "/api/get-balance", bytes.NewBuffer(reqBody))
	rr = httptest.NewRecorder()
	getBalanceHandler(rr, req)
	assert.Equal(t, http.StatusOK, rr.Code)
	var res2 []WalletBalance
	json.NewDecoder(rr.Body).Decode(&res2)
	assert.Equal(t, uint64(1), res2[0].Balance) // Still 1, from cache
	assert.Equal(t, 1, callCount)                // Call count should not increase

	// Wait for cache to expire
	time.Sleep(cacheTTL + 1*time.Second)

	// Third request (after TTL): should fetch again
	req = httptest.NewRequest(http.MethodPost, "/api/get-balance", bytes.NewBuffer(reqBody))
	rr = httptest.NewRecorder()
	getBalanceHandler(rr, req)
	assert.Equal(t, http.StatusOK, rr.Code)
	var res3 []WalletBalance
	json.NewDecoder(rr.Body).Decode(&res3)
	assert.Equal(t, uint64(2), res3[0].Balance) // Now 2, new fetch
	assert.Equal(t, 2, callCount)                // Call count should increase
}

func TestGetBalanceHandler_ConcurrentRequests(t *testing.T) {
	// Reset cache and in-flight requests for clean test runs
	walletCacheMutex.Lock()
	walletCache = make(map[string]*CacheEntry)
	walletCacheMutex.Unlock()

	inflightRequestsMutex.Lock()
	inflightRequests = make(map[string]*request)
	inflightRequestsMutex.Unlock()

	fetchCount := 0
	mockRPCClient := &MockSolanaRPCClient{
		GetBalanceFunc: func(ctx context.Context, account solana.PublicKey, commitment rpc.CommitmentType) (*rpc.GetBalanceResult, error) {
			time.Sleep(50 * time.Millisecond) // Simulate network delay
			fetchCount++
			return &rpc.GetBalanceResult{Value: uint64(fetchCount)}, nil
		},
	}
	solanaRPCClient = mockRPCClient

	testWallet := "HXt8LibraryaXz2y222y222y222y222y222y222y222y"
	reqBody, _ := json.Marshal(GetBalanceRequest{Wallets: []string{testWallet}})

	var wg sync.WaitGroup
	numRequests := 5
	responses := make(chan []WalletBalance, numRequests)

	for i := 0; i < numRequests; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodPost, "/api/get-balance", bytes.NewBuffer(reqBody))
			rr := httptest.NewRecorder()
			getBalanceHandler(rr, req)
			assert.Equal(t, http.StatusOK, rr.Code)
			var res []WalletBalance
			json.NewDecoder(rr.Body).Decode(&res)
			responses <- res
		}()
	}

	wg.Wait()
	close(responses)

	// All responses should have the same balance (from the first fetch)
	// And fetchCount should be 1, meaning only one actual fetch occurred.
	assert.Equal(t, 1, fetchCount, "Only one actual balance fetch should occur for concurrent requests")

	for res := range responses {
		assert.Len(t, res, 1)
		assert.Equal(t, uint64(1), res[0].Balance, "All concurrent requests should return the same cached balance")
	}
}

func TestGetBalanceHandler_Success(t *testing.T) {
	// Reset cache and in-flight requests for clean test runs
	walletCacheMutex.Lock()
	walletCache = make(map[string]*CacheEntry)
	walletCacheMutex.Unlock()

	inflightRequestsMutex.Lock()
	inflightRequests = make(map[string]*request)
	inflightRequestsMutex.Unlock()

	// Mock the Solana RPC client
	mockRPCClient := &MockSolanaRPCClient{
		GetBalanceFunc: func(ctx context.Context, account solanago.PublicKey, commitment rpc.CommitmentType) (*rpc.GetBalanceResult, error) {
			switch account.String() {
			case "validWallet1":
				return &rpc.GetBalanceResult{Value: 1000}, nil
			case "validWallet2":
				return &rpc.GetBalanceResult{Value: 2000}, nil
			default:
				return &rpc.GetBalanceResult{Value: 0}, nil
			}
		},
	}
	solanaRPCClient = mockRPCClient

	tests := []struct {
		name           string
		requestBody    GetBalanceRequest
		expectedStatus int
		expectedWallets []string
		expectedBalances []uint64
	}{
		{
			name: "Single wallet",
			requestBody: GetBalanceRequest{
				Wallets: []string{"validWallet1"},
			},
			expectedStatus: http.StatusOK,
			expectedWallets: []string{"validWallet1"},
			expectedBalances: []uint64{1000},
		},
		{
			name: "Multiple wallets",
			requestBody: GetBalanceRequest{
				Wallets: []string{"validWallet1", "validWallet2"},
			},
			expectedStatus: http.StatusOK,
			expectedWallets: []string{"validWallet1", "validWallet2"},
			expectedBalances: []uint64{1000, 2000},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			body, _ := json.Marshal(tt.requestBody)
			req := httptest.NewRequest(http.MethodPost, "/api/get-balance", bytes.NewBuffer(body))
			rr := httptest.NewRecorder()

			getBalanceHandler(rr, req)

			assert.Equal(t, tt.expectedStatus, rr.Code)

			var responses []WalletBalance
			err := json.NewDecoder(rr.Body).Decode(&responses)
			assert.NoError(t, err)
			assert.Len(t, responses, len(tt.expectedWallets))

			for i, expectedWallet := range tt.expectedWallets {
				found := false
				for _, res := range responses {
					if res.Wallet == expectedWallet {
						assert.Equal(t, tt.expectedBalances[i], res.Balance)
						assert.Empty(t, res.Error)
						found = true
						break
					}
				}
				assert.True(t, found, "Wallet %s not found in response", expectedWallet)
			}
		})
	}
}

func TestGetBalanceHandler_InvalidMethod(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/api/get-balance", nil)
	rr := httptest.NewRecorder()

	getBalanceHandler(rr, req)

	assert.Equal(t, http.StatusMethodNotAllowed, rr.Code)
}

func TestGetBalanceHandler_InvalidBody(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/api/get-balance", bytes.NewBufferString("invalid json"))
	rr := httptest.NewRecorder()

	getBalanceHandler(rr, req)

	assert.Equal(t, http.StatusBadRequest, rr.Code)
}

func TestGetBalanceHandler_EmptyWallets(t *testing.T) {
	body, _ := json.Marshal(GetBalanceRequest{Wallets: []string{}})
	req := httptest.NewRequest(http.MethodPost, "/api/get-balance", bytes.NewBuffer(body))
	rr := httptest.NewRecorder()

	getBalanceHandler(rr, req)

	assert.Equal(t, http.StatusBadRequest, rr.Code)
}

func TestGetBalanceHandler_Caching(t *testing.T) {
	// Reset cache and in-flight requests for clean test runs
	walletCacheMutex.Lock()
	walletCache = make(map[string]*CacheEntry)
	walletCacheMutex.Unlock()

	inflightRequestsMutex.Lock()
	inflightRequests = make(map[string]*request)
	inflightRequestsMutex.Unlock()

	callCount := 0
	mockRPCClient := &MockSolanaRPCClient{
		GetBalanceFunc: func(ctx context.Context, account solanago.PublicKey, commitment rpc.CommitmentType) (*rpc.GetBalanceResult, error) {
			callCount++
			return &rpc.GetBalanceResult{Value: uint64(callCount)}, nil // Return a unique value to track calls
		},
	}
	solanaRPCClient = mockRPCClient

	testWallet := "cacheTestWallet"
	reqBody, _ := json.Marshal(GetBalanceRequest{Wallets: []string{testWallet}})

	// First request: should fetch and cache
	req := httptest.NewRequest(http.MethodPost, "/api/get-balance", bytes.NewBuffer(reqBody))
	rr := httptest.NewRecorder()
	getBalanceHandler(rr, req)
	assert.Equal(t, http.StatusOK, rr.Code)
	var res1 []WalletBalance
	json.NewDecoder(rr.Body).Decode(&res1)
	assert.Equal(t, uint64(1), res1[0].Balance)
	assert.Equal(t, 1, callCount)

	// Second request (within TTL): should use cache
	req = httptest.NewRequest(http.MethodPost, "/api/get-balance", bytes.NewBuffer(reqBody))
	rr = httptest.NewRecorder()
	getBalanceHandler(rr, req)
	assert.Equal(t, http.StatusOK, rr.Code)
	var res2 []WalletBalance
	json.NewDecoder(rr.Body).Decode(&res2)
	assert.Equal(t, uint64(1), res2[0].Balance) // Still 1, from cache
	assert.Equal(t, 1, callCount)                // Call count should not increase

	// Wait for cache to expire
	time.Sleep(cacheTTL + 1*time.Second)

	// Third request (after TTL): should fetch again
	req = httptest.NewRequest(http.MethodPost, "/api/get-balance", bytes.NewBuffer(reqBody))
	rr = httptest.NewRecorder()
	getBalanceHandler(rr, req)
	assert.Equal(t, http.StatusOK, rr.Code)
	var res3 []WalletBalance
	json.NewDecoder(rr.Body).Decode(&res3)
	assert.Equal(t, uint64(2), res3[0].Balance) // Now 2, new fetch
	assert.Equal(t, 2, callCount)                // Call count should increase
}

func TestGetBalanceHandler_ConcurrentRequests(t *testing.T) {
	// Reset cache and in-flight requests for clean test runs
	walletCacheMutex.Lock()
	walletCache = make(map[string]*CacheEntry)
	walletCacheMutex.Unlock()

	inflightRequestsMutex.Lock()
	inflightRequests = make(map[string]*request)
	inflightRequestsMutex.Unlock()

	fetchCount := 0
	mockRPCClient := &MockSolanaRPCClient{
		GetBalanceFunc: func(ctx context.Context, account solanago.PublicKey, commitment rpc.CommitmentType) (*rpc.GetBalanceResult, error) {
			time.Sleep(50 * time.Millisecond) // Simulate network delay
			fetchCount++
			return &rpc.GetBalanceResult{Value: uint64(fetchCount)}, nil
		},
	}
	solanaRPCClient = mockRPCClient

	testWallet := "concurrentWallet"
	reqBody, _ := json.Marshal(GetBalanceRequest{Wallets: []string{testWallet}})

	var wg sync.WaitGroup
	numRequests := 5
	responses := make(chan []WalletBalance, numRequests)

	for i := 0; i < numRequests; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodPost, "/api/get-balance", bytes.NewBuffer(reqBody))
			rr := httptest.NewRecorder()
			getBalanceHandler(rr, req)
			assert.Equal(t, http.StatusOK, rr.Code)
			var res []WalletBalance
			json.NewDecoder(rr.Body).Decode(&res)
			responses <- res
		}()
	}

	wg.Wait()
	close(responses)

	// All responses should have the same balance (from the first fetch)
	// And fetchCount should be 1, meaning only one actual fetch occurred.
	assert.Equal(t, 1, fetchCount, "Only one actual balance fetch should occur for concurrent requests")

	for res := range responses {
		assert.Len(t, res, 1)
		assert.Equal(t, uint64(1), res[0].Balance, "All concurrent requests should return the same cached balance")
	}
}