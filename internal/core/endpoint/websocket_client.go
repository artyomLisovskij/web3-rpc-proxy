package endpoint

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/DODOEX/web3rpcproxy/internal/common"
	"github.com/DODOEX/web3rpcproxy/internal/core/rpc"
	"github.com/DODOEX/web3rpcproxy/utils/helpers"
	"github.com/duke-git/lancet/v2/slice"
	"github.com/gorilla/websocket"
	"github.com/rs/zerolog"
)

type websocketClientConfig struct {
	Transport     *http.Transport
	JSONRPCSchema *rpc.JSONRPCSchema
}

type websocketClient struct {
	logger   zerolog.Logger
	endpoint *Endpoint
	conn     *websocket.Conn
	sessions sync.Map
	ctx      context.Context
	cancel   context.CancelFunc
	mu       sync.Mutex
	config   *websocketClientConfig
}

// background() is a constant goroutine used to listen for websocket result messages
// request() is used to send a websocket request and wait for the result, the result is returned through the channel associated with the request's id
// Therefore, the Call() method will not have unformatted exceptions such as http pages, text, parsing errors, etc. They will all be considered connection errors
func NewWebSocketClient(endpoint *Endpoint, config *websocketClientConfig) Client {
	url := endpoint.Url().String()
	logger := zerolog.New(zerolog.ConsoleWriter{Out: os.Stdout}).With().Timestamp().
		Str("name", "web socket endpoint").
		Str("url", url).
		Logger()
	if url == "" {
		return nil
	}

	e := &websocketClient{
		endpoint: endpoint,
		logger:   logger,
		config:   config,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	e.conn = e.connect(ctx)
	if e.conn == nil {
		e = nil
		return nil
	}

	return e
}

func getJSONResultKey(data []rpc.JSONRPCResulter) string {
	ids := slice.Map(data, func(i int, result rpc.JSONRPCResulter) string {
		return fmt.Sprint(result.Raw()["id"])
	})
	slice.Sort(ids)
	return helpers.Short(slice.Join(ids, ""))
}

func background(ctx context.Context, logger zerolog.Logger, conn *websocket.Conn, sessions *sync.Map) {
	if conn == nil {
		logger.Error().Msg("WebSocket connection is nil")
		return
	}

	defer func() {
		if err := recover(); err != nil {
			logger.Error().Interface("error", err).Msg("Failed to receive message")
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return
		default:
			messageType, message, err := conn.ReadMessage()
			if err != nil {
				logger.Warn().Msgf("Error reading message: %v", err)
				// Check if the error is a result of connection closure
				if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
					return
				}
				continue
			}

			if messageType != websocket.TextMessage {
				logger.Warn().Msgf("Unexpected message type: %d", messageType)
				continue
			}

			// Parse the response
			results, _, err := rpc.UnmarshalJSONRPCResults(message)
			if err != nil {
				logger.Error().Err(err).Str("message", string(message)).Msg("Failed to unmarshal response")
				continue
			}

			// Log response details
			logger.Info().
				Str("raw_response", string(message)).
				Interface("results", results).
				Msg("Received WebSocket response")

			// Check results for null
			for _, result := range results {
				if result.Result() == nil {
					logger.Info().
						Str("method", "eth_blockNumber"). // Add method for filtering
						Interface("id", result.ID()).
						Interface("raw_response", result.Raw()).
						Msg("Received null result from WebSocket endpoint")
				}
			}

			// Get key for session search
			key := getJSONResultKey(results)
			if v, ok := sessions.Load(key); ok {
				if ch, ok := v.(chan []rpc.JSONRPCResulter); ok && ch != nil {
					logger.Debug().
						Str("key", key).
						Interface("results", results).
						Msg("Sending results to channel")
					ch <- results
				}
			} else {
				logger.Warn().
					Str("key", key).
					Interface("results", results).
					Msg("No session found for response")
			}
		}
	}
}

func (e *websocketClient) connect(ctx context.Context) *websocket.Conn {
	// Set request headers
	headers := make(http.Header)
	headers.Set("Content-Type", "application/json")
	if _headers := e.endpoint.Headers(); _headers != nil {
		for key, value := range _headers {
			headers.Set(key, value)
		}
	}

	dialer := websocket.Dialer{
		EnableCompression: true,
	}
	if e.config.Transport != nil {
		dialer.TLSClientConfig = e.config.Transport.TLSClientConfig.Clone()
	}

	// Connect to URL, ensuring the use of ws or wss:// prefix
	// Dial function is used to connect to the WebSocket server
	_connect := func(ctx context.Context) *websocket.Conn {
		var (
			duration int64 = 0
			health   bool  = false
		)

		defer func() {
			ops := []Attributer{
				WithAttr(Health, health),
				WithAttr(LastUpdateTime, time.Now()),
			}
			if duration > 0 {
				ops = append(ops, WithAttr(Duration, float64(duration)))
			}
			e.endpoint.Update(ops...)
		}()

		now := time.Now()
		conn, _, err := dialer.DialContext(ctx, e.endpoint.Url().String(), headers)
		if err != nil {
			e.logger.Error().Msgf("Error creating connection: %v", err)
			return nil
		}

		duration = time.Since(now).Milliseconds()
		health = true

		// Start listening wss, sending messages, receiving messages
		_ctx, _cancel := context.WithCancel(context.Background())
		e.ctx = _ctx
		e.cancel = _cancel
		go background(e.ctx, e.logger, conn, &e.sessions)

		return conn
	}

	// Connect
	conn := _connect(ctx)

	if conn == nil {
		return nil
	}

	// Set close callback, for reconnection
	conn.SetCloseHandler(func(code int, text string) error {
		e.logger.Warn().Msgf("Closing connection code %d and text %s", code, text)

		e.endpoint.Update(
			WithAttr(Health, false),
			WithAttr(LastUpdateTime, time.Now()),
		)

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		oldConn, oldCancel := e.conn, e.cancel
		// Reconnect
		e.conn = _connect(ctx)
		cancel()

		message := websocket.FormatCloseMessage(code, "")
		oldConn.WriteControl(websocket.CloseMessage, message, time.Now().Add(time.Second))
		time.AfterFunc(time.Second, func() {
			oldConn.Close()
			oldCancel()
		})
		return nil
	})

	return conn
}

func (e *websocketClient) Close() error {
	if e == nil || e.conn == nil {
		return nil
	}

	e.mu.Lock()
	defer e.mu.Unlock()

	if e.cancel != nil {
		e.cancel()
	}

	message := websocket.FormatCloseMessage(websocket.CloseNormalClosure, "")
	if err := e.conn.WriteControl(websocket.CloseMessage, message, time.Now().Add(time.Second)); err != nil {
		e.logger.Error().Err(err).Msg("Error sending close message")
	}

	if err := e.conn.Close(); err != nil {
		e.logger.Error().Err(err).Msg("Error closing connection")
		return err
	}

	return nil
}

// Check if the error is a result of connection closure
func isBrokenPipeError(err error) bool {
	var netErr *net.OpError
	if errors.As(err, &netErr) {
		if netErr.Op == "write" && netErr.Err.Error() == "write: broken pipe" {
			return true
		}
	}
	return false
}

func (e *websocketClient) request(ctx context.Context, key string, b []byte) ([]rpc.JSONRPCResulter, common.HTTPErrors) {
	if e == nil || e.conn == nil {
		return nil, common.UpstreamServerError("WebSocket connection is nil", nil)
	}

	c := make(chan []rpc.JSONRPCResulter)
	e.sessions.Store(key, c)
	defer func() {
		if c, ok := e.sessions.Load(key); ok {
			e.sessions.Delete(key)
			if ch, ok := c.(chan []rpc.JSONRPCResulter); ok && ch != nil {
				close(ch)
			}
		}
		_EndpointGauge(e.endpoint).Dec()
	}()

	_EndpointGauge(e.endpoint).Inc()
	e.mu.Lock()
	e.logger.Debug().Str("request_body", string(b)).Msg("Sending WebSocket request")
	err := e.conn.WriteMessage(websocket.TextMessage, b)
	e.mu.Unlock()

	if err != nil {
		e.logger.Warn().Msgf("Creating request %s %s", e.endpoint.Url(), string(b))
		e.logger.Error().Msgf("Error creating request: %v", err)

		if isBrokenPipeError(err) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			e.mu.Lock()
			conn := e.connect(ctx)
			if conn != nil {
				e.conn = conn
				// Repeat sending message after reconnection
				err = e.conn.WriteMessage(websocket.TextMessage, b)
			}
			e.mu.Unlock()
			cancel()

			if err == nil {
				// If repeated sending is successful, wait for the answer
				select {
				case results, ok := <-c:
					if !ok {
						return nil, common.UpstreamServerError("Error connection to endpoint", nil)
					}
					e.logger.Debug().
						Interface("results", results).
						Str("request_body", string(b)).
						Msg("Received response after reconnect")
					return results, nil
				case <-ctx.Done():
					return nil, common.TimeoutError("context deadline exceeded")
				}
			}
		}

		if _err, ok := err.(*net.OpError); ok {
			return nil, common.UpstreamServerError("Error creating request", _err.Err)
		} else {
			return nil, common.UpstreamServerError("Error creating request", err)
		}
	}

	select {
	case results, ok := <-c:
		if !ok {
			return nil, common.UpstreamServerError("Error connection to endpoint", nil)
		}
		e.logger.Debug().
			Interface("results", results).
			Str("request_body", string(b)).
			Str("key", key).
			Msg("Received WebSocket response")

		// Check results for null
		if len(results) > 0 {
			for _, result := range results {
				if result.Result() == nil {
					e.logger.Warn().
						Interface("result", result.Raw()).
						Str("request_body", string(b)).
						Msg("Received null result from upstream")
				}
			}
		}

		return results, nil
	case <-ctx.Done():
		return nil, common.TimeoutError("context deadline exceeded")
	}
}

func getJSONRPCKey(data []rpc.SealedJSONRPC) string {
	ids := slice.Map(data, func(i int, jsonrpc rpc.SealedJSONRPC) string {
		return fmt.Sprint(jsonrpc.ID)
	})
	slice.Sort(ids)
	return helpers.Short(slice.Join(ids, ""))
}

func init() {
	// Open log file in the root directory
	_, err := os.OpenFile("/app/websocket.log", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		fmt.Printf("Error opening log file: %v\n", err)
		return
	}
}

func (e *websocketClient) Call(ctx context.Context, data []rpc.SealedJSONRPC, profiles ...*common.ResponseProfile) (results []rpc.JSONRPCResulter, err error) {
	b, err := json.Marshal(data)
	if err != nil {
		return nil, common.InternalServerError("Marshalling request failed", err)
	}

	// Direct output to stdout
	fmt.Printf("🚀 Sending request: %s\n", string(b))

	var profile = &common.ResponseProfile{}
	if len(profiles) > 0 {
		profile = profiles[0]
	}

	// Request
	now, key := time.Now(), getJSONRPCKey(data)
	results, err = e.request(ctx, key, b)
	profile.Duration = time.Since(now).Milliseconds()

	if err != nil {
		fmt.Printf("❌ Request error: %v\n", err)
		switch err.Error() {
		case "Error connection to endpoint":
			profile.Code = "connection_error"
		case "Error creating request":
			profile.Code = "request_error"
		}
		profile.Error = err.Error()
		return nil, err
	}

	fmt.Printf("📥 Received response: %+v\n", results)

	body, err := json.Marshal(results)
	if err == nil {
		profile.Status = 200
		profile.Traffic = len(body)
	}

	if len(results) > 0 {
		if r := results[len(results)-1]; r.Type() == rpc.JSONRPC_ERROR {
			fmt.Printf("⚠️ Error received: %+v\n", r.Error())
			recordingErrorResult(profile, r)
			return results, nil
		}

		// Check ID and results correspondence
		for i, result := range results {
			if i < len(data) {
				fmt.Printf("🔍 Checking result:\n")
				fmt.Printf("   Method: %s\n", data[i].Method)
				fmt.Printf("   Request ID: %v\n", data[i].ID)
				fmt.Printf("   Response ID: %v\n", result.ID())
				fmt.Printf("   Result: %+v\n", result.Result())

				if result.Result() == nil {
					fmt.Printf("⚠️ Null result received:\n")
					fmt.Printf("   Method: %s\n", data[i].Method)
					fmt.Printf("   Request: %+v\n", data[i])
					fmt.Printf("   Response: %+v\n", result.Raw())
				}
			}
		}
	}

	fmt.Printf("=== End WebSocket Debug Log ===\n\n")
	return results, nil
}
