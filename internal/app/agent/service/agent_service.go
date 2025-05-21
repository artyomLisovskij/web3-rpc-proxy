package service

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/DODOEX/web3rpcproxy/internal/common"
	"github.com/DODOEX/web3rpcproxy/internal/core"
	"github.com/DODOEX/web3rpcproxy/internal/core/endpoint"
	"github.com/DODOEX/web3rpcproxy/internal/core/reqctx"
	"github.com/DODOEX/web3rpcproxy/internal/core/rpc"
	"github.com/DODOEX/web3rpcproxy/utils"
	"github.com/DODOEX/web3rpcproxy/utils/config"
	"github.com/DODOEX/web3rpcproxy/utils/helpers"
	"github.com/allegro/bigcache"
	"github.com/rs/zerolog"
)

type CacheMethodConfig struct {
	TTL               time.Duration `json:"ttl"`
	InvalidateOnBlock bool          `json:"invalidate_on_block"`
}

type CacheEntry struct {
	V          any
	T          int64
	BlockNum   *string // Block number at which the request was made
	compressed bool
}

type agentServiceConfig struct {
	CacheMethods      map[string]CacheMethodConfig
	MaxEntryCacheSize int
	DisableCache      bool
}

// AgentService
type agentService struct {
	logger     zerolog.Logger
	client     core.Client
	es         endpoint.Selector
	jrpcSchema *rpc.JSONRPCSchema
	cache      *bigcache.BigCache
	config     *agentServiceConfig
}

// define interface of IAgentService
//
//go:generate mockgen -destination=agent_service_mock.go -package=service . AgentService
type AgentService interface {
	Call(ctx context.Context, reqctx reqctx.Reqctxs, endpoints []*endpoint.Endpoint) ([]byte, error)
}

func nearestPowerOfTwo(n uint) uint {
	if n == 0 {
		return 1
	}

	n--
	n |= n >> 1
	n |= n >> 2
	n |= n >> 4
	n |= n >> 8
	n |= n >> 16
	n++

	return n
}

// init AgentService
func NewAgentService(
	logger zerolog.Logger,
	config *config.Conf,
	jrpcSchema *rpc.JSONRPCSchema,
	client core.Client,
	endpointService EndpointService,
) AgentService {
	logger = logger.With().Str("name", "agent_service").Logger()

	existExpiryConfig := config.Exists("cache.results.expiry_durations")
	_config := &agentServiceConfig{
		DisableCache:      config.Bool("cache.results.disable", false) || !existExpiryConfig,
		MaxEntryCacheSize: 512 * 1024, // 512KB
	}

	if existExpiryConfig {
		expiryConfig := map[string]CacheMethodConfig{}
		config.Unmarshal("cache.results.expiry_durations", &expiryConfig)
		_config.CacheMethods = expiryConfig
	}

	// default cache size is 512MB
	totalCacheSize := config.Int("cache.results.size", 512*1024*1024)

	shards := int(nearestPowerOfTwo(uint(len(endpointService.Chains()))))
	// must have 8MB size pre shard
	if totalCacheSize/shards < 8 {
		shards = int(nearestPowerOfTwo(uint(totalCacheSize / 8)))
	}

	_cacheConfig := bigcache.Config{
		// number of shards (must be a power of 2)
		Shards: shards,

		// time after which entry can be evicted
		LifeWindow: 15 * time.Minute,

		// Interval between removing expired entries (clean up).
		// If set to <= 0 then no action is p4erformed.
		// Setting to < 1 second is counterproductive — bigcache has a one second resolution.
		CleanWindow: 15 * time.Minute,

		// rps * lifeWindow, used only in initial memory allocation
		// MaxEntriesInWindow: 1000 * 10 * 60,

		// max entry size in bytes, used only in initial memory allocation
		MaxEntrySize: _config.MaxEntryCacheSize,

		// prints information about additional memory allocation
		// Verbose: true,

		// cache will not allocate more memory than this limit, value in MB
		// if value is reached then the oldest entries can be overridden for the new ones
		// 0 value means no size limit
		HardMaxCacheSize: totalCacheSize / 1024 / 1024,
	}
	config.Unmarshal("agent.bigcache", &_cacheConfig)

	cache, initErr := bigcache.NewBigCache(_cacheConfig)
	if initErr != nil {
		log.Fatal(initErr)
	}

	logger.Info().Msgf("Cache size: %d MB", _cacheConfig.HardMaxCacheSize)

	service := agentService{
		config:     _config,
		client:     client,
		logger:     logger,
		jrpcSchema: jrpcSchema,
		cache:      cache,
		es:         endpoint.NewSelector(),
	}

	return service
}

func (a agentService) Call(ctx context.Context, rc reqctx.Reqctxs, endpoints []*endpoint.Endpoint) ([]byte, error) {
	// 1. Parse JSON-RPC array
	jsonrpcs, isBatchCall, err := rpc.UnmarshalJSONRPCs(*rc.Body())
	if err != nil {
		a.logger.Error().Err(err).Str("body", string(*rc.Body())).Msg("Failed to unmarshal JSON-RPC request")
		return nil, common.BadRequestError(err.Error(), err)
	}

	// Log incoming request
	a.logger.Info().
		Bool("is_batch", isBatchCall).
		Str("raw_request", string(*rc.Body())).
		Interface("parsed_requests", jsonrpcs).
		Msg("Processing JSON-RPC request")

	if len(jsonrpcs) == 0 {
		if isBatchCall {
			return json.Marshal([]interface{}{})
		}
		return json.Marshal(map[string]interface{}{
			"jsonrpc": "2.0",
			"id":      nil,
			"result":  nil,
		})
	}

	// Validate requests
	for i := range jsonrpcs {
		if err := a.jrpcSchema.ValidateRequest(jsonrpcs[i].Method(), jsonrpcs[i].Raw()); err != nil {
			a.logger.Error().
				Err(err).
				Str("method", jsonrpcs[i].Method()).
				Interface("params", jsonrpcs[i].Raw()).
				Msg("Invalid JSON-RPC request")
			return nil, common.BadRequestError(err.Error(), err)
		}

		// Log details of each request
		a.logger.Info().
			Str("method", jsonrpcs[i].Method()).
			Interface("id", jsonrpcs[i].ID()).
			Interface("params", jsonrpcs[i].Params()).
			Msg("Validated JSON-RPC request")
	}

	// Track which results came from cache
	cacheHits := make([]bool, len(jsonrpcs))

	// Send request
	dispatch := func(data []rpc.JSONRPCer) (interface{}, error) {
		if len(data) == 0 {
			if isBatchCall {
				return []interface{}{}, nil
			}
			return map[string]interface{}{
				"jsonrpc": "2.0",
				"id":      nil,
				"result":  nil,
			}, nil
		}

		// Execute request
		results, err := a.call(ctx, rc, endpoints, data)
		if err != nil {
			a.logger.Error().Err(err).Msg("RPC call failed")
			return nil, err
		}

		// Log results
		a.logger.Info().
			Interface("results", results).
			Msg("Received RPC results")

		// For batch requests, return array of results
		if isBatchCall {
			responseArray := make([]interface{}, len(results))
			for i, result := range results {
				responseArray[i] = map[string]interface{}{
					"jsonrpc":    "2.0",
					"id":         result.ID,
					"result":     result.Result,
					"error":      result.Error,
					"from_cache": cacheHits[i],
				}
			}
			return responseArray, nil
		}

		// For single requests, return first result
		if len(results) > 0 {
			return map[string]interface{}{
				"jsonrpc":    "2.0",
				"id":         results[0].ID,
				"result":     results[0].Result,
				"error":      results[0].Error,
				"from_cache": cacheHits[0],
			}, nil
		}

		return map[string]interface{}{
			"jsonrpc":    "2.0",
			"id":         nil,
			"result":     nil,
			"from_cache": false,
		}, nil
	}

	// Process request
	handle := func(_jsonrpcs []rpc.JSONRPCer) ([]byte, error) {
		results, err := dispatch(_jsonrpcs)
		if err != nil {
			return nil, err
		}

		return json.Marshal(results)
	}

	// Check caching
	chainId := rc.ChainID()
	appName := "unknown"
	if rc.App() != nil {
		appName = rc.App().Name
	}

	// Check if caching is disabled
	if a.config.DisableCache {
		a.logger.Info().
			Uint64("chain_id", chainId).
			Str("app", appName).
			Bool("cache_disabled", a.config.DisableCache).
			Msg("Cache globally disabled, making direct request")
		return handle(jsonrpcs)
	}

	// Check caching settings in request
	if !rc.Options().Caches() {
		a.logger.Info().
			Uint64("chain_id", chainId).
			Str("app", appName).
			Bool("option_caches", rc.Options().Caches()).
			Msg("Cache disabled by request options, making direct request")
		return handle(jsonrpcs)
	}

	// 3. Use cache
	var (
		results   = make([]rpc.SealedJSONRPCResult, len(jsonrpcs))
		_jsonrpcs = []rpc.JSONRPCer{}
		mapping   = map[string][]int{}
	)

	// Process requests, look in cache
	for i := range jsonrpcs {
		method := jsonrpcs[i].Method()
		if !a.canCache(method) {
			a.logger.Info().
				Uint64("chain_id", chainId).
				Str("app", appName).
				Str("method", method).
				Msg("Method not cacheable, making direct request")
			_jsonrpcs = append(_jsonrpcs, jsonrpcs[i])
			utils.TotalCaches.WithLabelValues(fmt.Sprint(chainId), appName, method, "skip").Inc()
			cacheHits[i] = false
			continue
		}

		// Try to get from cache
		key := a.cacheKey(chainId, jsonrpcs[i])
		if value, err := a.getCache(key, method, nil); err == nil {
			results[i] = rpc.SealedJSONRPCResult{
				ID:        jsonrpcs[i].Raw()["id"],
				Version:   jsonrpcs[i].Version(),
				Result:    value,
				FromCache: true,
			}
			cacheHits[i] = true
			a.logger.Debug().
				Str("method", method).
				Str("key", key).
				Msg("Cache hit")
		} else {
			_jsonrpcs = append(_jsonrpcs, jsonrpcs[i])
			mapping[jsonrpcs[i].ID()] = append(mapping[jsonrpcs[i].ID()], i)
			cacheHits[i] = false
			a.logger.Debug().
				Err(err).
				Str("method", method).
				Str("key", key).
				Msg("Cache miss")
		}
	}

	// Return results from cache
	if len(_jsonrpcs) <= 0 {
		a.logger.Info().
			Uint64("chain_id", chainId).
			Str("app", appName).
			Msg("All results from cache")
		if isBatchCall {
			responseArray := make([]interface{}, len(results))
			for i, result := range results {
				responseArray[i] = map[string]interface{}{
					"jsonrpc":    "2.0",
					"id":         result.ID,
					"result":     result.Result,
					"error":      result.Error,
					"from_cache": cacheHits[i],
				}
				// Log each result
				a.logger.Info().
					Uint64("chain_id", chainId).
					Str("app", appName).
					Str("method", jsonrpcs[i].Method()).
					Interface("id", result.ID).
					Bool("from_cache", cacheHits[i]).
					Msg("Response details")
			}
			return json.Marshal(responseArray)
		} else if len(results) > 0 {
			// Log result
			a.logger.Info().
				Uint64("chain_id", chainId).
				Str("app", appName).
				Str("method", jsonrpcs[0].Method()).
				Interface("id", results[0].ID).
				Bool("from_cache", cacheHits[0]).
				Msg("Response details")
			return json.Marshal(map[string]interface{}{
				"jsonrpc":    "2.0",
				"id":         results[0].ID,
				"result":     results[0].Result,
				"error":      results[0].Error,
				"from_cache": cacheHits[0],
			})
		}
	}

	// Make request for missing cache data
	data, err := dispatch(_jsonrpcs)
	if err != nil {
		return nil, err
	}

	// Combine results from cache and request
	if _results, ok := data.([]interface{}); ok && isBatchCall {
		// For batch requests
		responseArray := make([]interface{}, len(results))
		for i := range results {
			if results[i].Result != nil {
				// Take from cache
				responseArray[i] = map[string]interface{}{
					"jsonrpc":    "2.0",
					"id":         results[i].ID,
					"result":     results[i].Result,
					"error":      results[i].Error,
					"from_cache": true,
				}
				// Log result from cache
				a.logger.Info().
					Uint64("chain_id", chainId).
					Str("app", appName).
					Str("method", jsonrpcs[i].Method()).
					Interface("id", results[i].ID).
					Bool("from_cache", true).
					Msg("Response details")
			}
		}
		// Add new results
		for _, result := range _results {
			if resultMap, ok := result.(map[string]interface{}); ok {
				id := fmt.Sprint(resultMap["id"])
				if indexes, ok := mapping[id]; ok {
					for _, index := range indexes {
						responseArray[index] = map[string]interface{}{
							"jsonrpc":    "2.0",
							"id":         resultMap["id"],
							"result":     resultMap["result"],
							"error":      resultMap["error"],
							"from_cache": false,
						}
						// Log result from request
						a.logger.Info().
							Uint64("chain_id", chainId).
							Str("app", appName).
							Str("method", jsonrpcs[index].Method()).
							Interface("id", id).
							Bool("from_cache", false).
							Msg("Response details")
					}
				}
			}
		}
		return json.Marshal(responseArray)
	} else if result, ok := data.(map[string]interface{}); ok && !isBatchCall {
		// For single request
		// Add from_cache field
		result["from_cache"] = false
		// Log result
		a.logger.Info().
			Uint64("chain_id", chainId).
			Str("app", appName).
			Str("method", jsonrpcs[0].Method()).
			Interface("id", result["id"]).
			Bool("from_cache", false).
			Msg("Response details")
		return json.Marshal(result)
	}

	return json.Marshal(data)
}

func (a agentService) call(ctx context.Context, rc reqctx.Reqctxs, endpoints []*endpoint.Endpoint, jsonrpcs []rpc.JSONRPCer) (results []rpc.SealedJSONRPCResult, err error) {
	chainId := rc.ChainID()
	// Get endpoints
	_endpoints, ok := a.es.Select(ctx, rc, endpoints, jsonrpcs)
	if !ok || len(_endpoints) <= 0 {
		a.logger.Error().Msgf("%d No available endpoints", chainId)
		return nil, common.InternalServerError("No available endpoints")
	}

	// Get current block number for caching
	currentBlock, err := a.getCurrentBlock(ctx, rc, _endpoints)
	if err != nil {
		a.logger.Warn().Err(err).Msg("Failed to get current block number")
	}

	// Form array of SealedJSONRPC for sending
	sealedRPCs := make([]rpc.SealedJSONRPC, len(jsonrpcs))
	for i, jr := range jsonrpcs {
		sealed := jr.Seal()
		// Save original ID
		sealed.ID = fmt.Sprint(jr.Raw()["id"])
		sealedRPCs[i] = sealed

		a.logger.Debug().
			Interface("original_id", jr.Raw()["id"]).
			Interface("sealed_id", sealed.ID).
			Msg("Processing request")
	}

	// Send request
	_results, err := a.client.Request(ctx, rc, _endpoints, sealedRPCs)
	if err != nil {
		return nil, err
	}

	// Transform results and save to cache
	results = make([]rpc.SealedJSONRPCResult, len(_results))
	for i, result := range _results {
		results[i] = rpc.SealedJSONRPCResult{
			ID:        result.Raw()["id"],
			Version:   result.Version(),
			Result:    result.Result(),
			Error:     result.Error(),
			FromCache: false,
		}

		// Check ID correspondence
		if i < len(jsonrpcs) {
			originalID := jsonrpcs[i].Raw()["id"]
			if fmt.Sprint(results[i].ID) != fmt.Sprint(originalID) {
				a.logger.Warn().
					Interface("original_id", originalID).
					Interface("result_id", results[i].ID).
					Msg("Response ID does not match request ID")
				results[i].ID = originalID
			}

			// Save result to cache
			method := jsonrpcs[i].Method()
			if a.canCache(method) && results[i].Error == nil {
				key := a.cacheKey(chainId, jsonrpcs[i])
				if err := a.setCache(key, results[i].Result, method, currentBlock); err != nil {
					a.logger.Warn().
						Err(err).
						Str("method", method).
						Str("key", key).
						Msg("Failed to cache result")
				} else {
					a.logger.Debug().
						Str("method", method).
						Str("key", key).
						Msg("Cached result")
				}
			}
		}
	}

	return results, nil
}

// canCache checks if the method can be cached
func (a agentService) canCache(method string) bool {
	if a.config.CacheMethods == nil {
		return false
	}
	_, ok := a.config.CacheMethods[method]
	return ok
}

// shouldInvalidateCache checks if the cache should be invalidated
func (a agentService) shouldInvalidateCache(method string, entry *CacheEntry, currentBlock *string) bool {
	if entry == nil || a.config.CacheMethods == nil {
		return true
	}

	config, ok := a.config.CacheMethods[method]
	if !ok {
		return true
	}

	// Check TTL
	if time.Since(time.Unix(entry.T, 0)) > config.TTL {
		return true
	}

	// Check block number if needed
	if config.InvalidateOnBlock && currentBlock != nil && entry.BlockNum != nil {
		return *currentBlock != *entry.BlockNum
	}

	return false
}

// cacheKey generates a cache key
func (a agentService) cacheKey(chainId uint64, jsonrpc rpc.JSONRPCer) string {
	return fmt.Sprintf("%d:%s:%s", chainId, jsonrpc.Method(), helpers.Short(fmt.Sprint(jsonrpc.Raw())))
}

// getCache retrieves a value from cache
func (a agentService) getCache(key string, method string, currentBlock *string) (interface{}, error) {
	data, err := a.cache.Get(key)
	if err == bigcache.ErrEntryNotFound {
		return nil, fmt.Errorf("cache entry not found")
	}
	if err != nil {
		return nil, err
	}

	var entry CacheEntry
	if err := json.Unmarshal(data, &entry); err != nil {
		return nil, err
	}

	// Check cache validity
	if a.shouldInvalidateCache(method, &entry, currentBlock) {
		return nil, nil
	}

	if entry.compressed {
		if decompressed, err := helpers.Decompress(entry.V.([]byte)); err != nil {
			return nil, err
		} else {
			var result interface{}
			if err := json.Unmarshal(decompressed, &result); err != nil {
				return nil, err
			}
			return result, nil
		}
	}

	return entry.V, nil
}

// setCache stores a value in cache
func (a agentService) setCache(key string, value interface{}, method string, currentBlock *string) error {
	if !a.canCache(method) {
		return nil
	}

	entry := CacheEntry{
		V:        value,
		T:        time.Now().Unix(),
		BlockNum: currentBlock,
	}

	// Check value size
	jsonData, err := json.Marshal(value)
	if err != nil {
		return err
	}

	// If size exceeds limit - compress
	if len(jsonData) > a.config.MaxEntryCacheSize {
		compressed, err := helpers.Compress(jsonData)
		if err != nil {
			return err
		}
		entry.V = compressed
		entry.compressed = true
	}

	data, err := json.Marshal(entry)
	if err != nil {
		return err
	}

	return a.cache.Set(key, data)
}

// getCurrentBlock gets the current block number
func (a agentService) getCurrentBlock(ctx context.Context, rc reqctx.Reqctxs, endpoints []*endpoint.Endpoint) (*string, error) {
	blockNumberRPC := rpc.NewJSONRPC(map[string]any{
		"method": "eth_blockNumber",
		"params": []any{},
		"id":     "block_number",
	})
	sealed := blockNumberRPC.Seal()

	results, err := a.client.Request(ctx, rc, endpoints, []rpc.SealedJSONRPC{sealed})
	if err != nil {
		return nil, err
	}

	if len(results) > 0 && results[0].Result() != nil {
		blockNum := fmt.Sprint(results[0].Result())
		return &blockNum, nil
	}

	return nil, nil
}
