package controller

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/DODOEX/web3rpcproxy/internal/common"
	"github.com/DODOEX/web3rpcproxy/internal/core/reqctx"
	"github.com/DODOEX/web3rpcproxy/utils"
	"github.com/fasthttp/websocket"
	"github.com/valyala/fasthttp"
)

const (
	writeWait      = 10 * time.Second    // Write timeout
	pongWait       = 60 * time.Second    // Timeout waiting for pong
	pingPeriod     = (pongWait * 9) / 10 // Period for sending ping
	maxMessageSize = 512 * 1024          // Maximum message size (512KB)
)

var (
	upgrader = websocket.FastHTTPUpgrader{
		ReadBufferSize:  1024,
		WriteBufferSize: 1024,
		CheckOrigin: func(ctx *fasthttp.RequestCtx) bool {
			return true // In production, origin check should be configured
		},
	}

	// WebSocket log file
	wsLogFile *os.File
)

func init() {
	// Open WebSocket log file
	var err error
	wsLogFile, err = os.OpenFile("logs/websocket.log", os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		fmt.Printf("Error opening WebSocket log file: %v\n", err)
	}
}

func logWebSocket(message string) {
	timestamp := time.Now().Format("2006-01-02 15:04:05.000")
	logMessage := fmt.Sprintf("[%s] %s\n", timestamp, message)

	// Write to file
	if wsLogFile != nil {
		if _, err := wsLogFile.WriteString(logMessage); err != nil {
			fmt.Printf("Error writing to WebSocket log file: %v\n", err)
		}
	}

	// Also output to stdout
	// fmt.Print(logMessage)
}

// HandleWebSocket handles WebSocket connections following the same logic as HandleCall
func (a *agentController) HandleWebSocket(ctx *fasthttp.RequestCtx) {
	logWebSocket("=== Start processing WebSocket connection ===")
	defer logWebSocket("=== End processing WebSocket connection ===")

	// Get chainId from request parameters
	chainIdStr := fmt.Sprint(ctx.UserValue("chain"))
	chainId, err := strconv.ParseUint(chainIdStr, 10, 64)
	if err != nil {
		logWebSocket(fmt.Sprintf("❌ Error parsing chainId: %v", err))
		a.logger.Error().Err(err).Msg("Invalid chainId")
		ctx.Error("Invalid chainId", fasthttp.StatusBadRequest)
		return
	}

	logWebSocket(fmt.Sprintf("🔗 Received WebSocket request for chain_id: %d", chainId))

	// Create WebSocket connection
	err = upgrader.Upgrade(ctx, func(conn *websocket.Conn) {
		if conn == nil {
			logWebSocket("❌ WebSocket connection is not established")
			a.logger.Error().Msg("WebSocket connection is nil")
			return
		}

		// Create context for proper goroutine management
		wsCtx, cancel := context.WithCancel(context.Background())
		defer cancel()

		// Mutex to protect concurrent writes to WebSocket
		var writeMutex sync.Mutex

		// Connection state tracking
		var connectionClosed bool
		var closeMutex sync.RWMutex

		// Helper function to safely write to WebSocket
		safeWrite := func(messageType int, data []byte) error {
			closeMutex.RLock()
			if connectionClosed {
				closeMutex.RUnlock()
				return fmt.Errorf("connection is closed")
			}
			closeMutex.RUnlock()

			writeMutex.Lock()
			defer writeMutex.Unlock()

			// Double-check after acquiring write lock
			closeMutex.RLock()
			if connectionClosed {
				closeMutex.RUnlock()
				return fmt.Errorf("connection is closed")
			}
			closeMutex.RUnlock()

			conn.SetWriteDeadline(time.Now().Add(writeWait))
			return conn.WriteMessage(messageType, data)
		}

		// Helper function to safely close connection
		safeClose := func() {
			closeMutex.Lock()
			if !connectionClosed {
				connectionClosed = true
				if err := conn.Close(); err != nil {
					logWebSocket(fmt.Sprintf("❌ Error closing connection: %v", err))
					a.logger.Error().Err(err).Msg("Error closing WebSocket connection")
				}
			}
			closeMutex.Unlock()
		}

		defer safeClose()

		logWebSocket("🔌 WebSocket connection established")

		// Configure connection parameters
		conn.SetReadLimit(maxMessageSize)
		conn.SetReadDeadline(time.Now().Add(pongWait))
		conn.SetPongHandler(func(string) error {
			logWebSocket("📍 Received PONG")
			conn.SetReadDeadline(time.Now().Add(pongWait))
			return nil
		})

		// Send welcome message
		welcomeMsg, _ := json.Marshal(map[string]string{"status": "connected"})
		if err := safeWrite(websocket.TextMessage, welcomeMsg); err != nil {
			logWebSocket(fmt.Sprintf("❌ Error sending welcome message: %v", err))
			a.logger.Error().Err(err).Msg("Failed to send welcome message")
			return
		}

		logWebSocket("👋 Sent welcome message")

		a.logger.Debug().Msgf("New WebSocket connection established for chainId=%v", chainId)

		// Start ping goroutine with proper context management
		go func() {
			ticker := time.NewTicker(pingPeriod)
			defer ticker.Stop()

			for {
				select {
				case <-wsCtx.Done():
					logWebSocket("📤 Ping goroutine stopped")
					return
				case <-ticker.C:
					logWebSocket("📤 Sending PING")
					if err := safeWrite(websocket.PingMessage, nil); err != nil {
						logWebSocket(fmt.Sprintf("❌ Error sending PING: %v", err))
						a.logger.Error().Err(err).Msg("Error sending ping")
						return
					}
				}
			}
		}()

		// Main loop for processing messages
		for {
			messageType, message, err := conn.ReadMessage()
			if err != nil {
				if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
					logWebSocket(fmt.Sprintf("❌ Unexpected connection closure: %v", err))
					a.logger.Error().Err(err).Msg("WebSocket read error")
				}
				break
			}

			// Skip non-text messages
			if messageType != websocket.TextMessage {
				logWebSocket(fmt.Sprintf("⚠️ Skipping non-text message type: %d", messageType))
				continue
			}

			logWebSocket(fmt.Sprintf("📥 Received message: %s", string(message)))

			a.logger.Debug().
				Str("raw_message", string(message)).
				Msg("Received WebSocket message")

			// Check if message is an array or single object
			var isBatch bool
			var requestID interface{}
			decoder := json.NewDecoder(bytes.NewReader(message))
			token, err := decoder.Token()
			if err != nil {
				logWebSocket(fmt.Sprintf("❌ Error parsing JSON: %v", err))
				a.logger.Error().Err(err).Str("message", string(message)).Msg("Failed to parse initial JSON token")
				errorResponse := map[string]interface{}{
					"jsonrpc": "2.0",
					"id":      nil,
					"error": map[string]interface{}{
						"code":    -32700,
						"message": "Parse error",
						"data":    err.Error(),
					},
				}
				responseBytes, _ := json.Marshal(errorResponse)
				if err := safeWrite(websocket.TextMessage, responseBytes); err != nil {
					logWebSocket(fmt.Sprintf("❌ Error sending error response: %v", err))
					break
				}
				continue
			}

			if delim, ok := token.(json.Delim); ok && delim == '[' {
				isBatch = true
				logWebSocket("📦 Received batch request")
				a.logger.Debug().Msg("Detected batch request")

				// Read entire array of requests
				var batchRequests []map[string]interface{}
				if err := decoder.Decode(&batchRequests); err == nil {
					if len(batchRequests) > 0 {
						requestID = batchRequests[0]["id"]
						a.logger.Debug().
							Interface("request_id", requestID).
							Interface("batch_size", len(batchRequests)).
							Msg("Parsed batch request")
					}
				}
			} else {
				logWebSocket("📝 Received single request")
				a.logger.Debug().Msg("Detected single request")
				// Decode entire request to get ID
				var request map[string]interface{}
				decoder = json.NewDecoder(bytes.NewReader(message))
				if err := decoder.Decode(&request); err == nil {
					requestID = request["id"]
					a.logger.Debug().
						Interface("request_id", requestID).
						Interface("request", request).
						Msg("Parsed single request")
				}
			}

			// Create request context with message body
			wsCtx := &fasthttp.RequestCtx{}
			wsCtx.Request.SetBody(message)
			// Set chain ID in context
			wsCtx.SetUserValue("chain", chainIdStr)

			rc := reqctx.NewReqctx(wsCtx, a.conf.Copy(), a.logger)
			if rc == nil {
				logWebSocket("❌ Failed to create request context")
				continue
			}

			// Set chain ID
			rc.SetChain(&common.Chain{ID: chainId})

			// Get endpoints
			endpoints, ok := a.endpointService.GetAll(chainId)
			if !ok || len(endpoints) == 0 {
				logWebSocket(fmt.Sprintf("❌ No available endpoints for chainId=%v", chainId))
				errorResponse := map[string]interface{}{
					"jsonrpc": "2.0",
					"id":      requestID,
					"error": map[string]interface{}{
						"code":    -32000,
						"message": "No endpoints available",
					},
				}
				responseBytes, _ := json.Marshal(errorResponse)
				if err := safeWrite(websocket.TextMessage, responseBytes); err != nil {
					logWebSocket(fmt.Sprintf("❌ Error sending error response: %v", err))
					break
				}
				continue
			}

			// Process RPC request
			startTime := time.Now().UnixMilli()
			responseBytes, err := a.agentService.Call(ctx, rc, endpoints)
			endTime := time.Now().UnixMilli()

			var queryStatus common.QueryStatus

			if err != nil {
				logWebSocket(fmt.Sprintf("❌ Error calling RPC: %v", err))
				if httpErr, ok := err.(common.HTTPErrors); ok {
					queryStatus = httpErr.QueryStatus()
				} else {
					queryStatus = common.Error
				}

				var errorResponse interface{}
				singleError := map[string]interface{}{
					"jsonrpc": "2.0",
					"id":      requestID,
					"error": map[string]interface{}{
						"code":    -32603,
						"message": err.Error(),
					},
				}

				if isBatch {
					errorResponse = []map[string]interface{}{singleError}
				} else {
					errorResponse = singleError
				}

				responseBytes, _ = json.Marshal(errorResponse)
				a.logger.Error().
					Str("error", err.Error()).
					Str("response", string(responseBytes)).
					Msg("Sending error response")
			} else {
				queryStatus = common.Success
				logWebSocket(fmt.Sprintf("📤 Sending response: %s", string(responseBytes)))
			}

			// Send response
			if err := safeWrite(websocket.TextMessage, responseBytes); err != nil {
				logWebSocket(fmt.Sprintf("❌ Error sending response: %v", err))
				break
			}

			// Update metrics
			appName := "unknown"
			if rc.App() != nil {
				appName = rc.App().Name
			}
			utils.TotalRequests.WithLabelValues(chainIdStr, appName, string(queryStatus)).Inc()
			utils.RequestDurations.WithLabelValues(chainIdStr, appName).Observe(float64(endTime-startTime) / 1000.0)

			// Publish statistics to AMQP if enabled
			if a.amqp != nil && a.amqp.Conn != nil && chainId != 0 {
				profile := &common.QueryProfile{
					Status:    queryStatus,
					Starttime: startTime,
					Endtime:   endTime,
				}
				go a.publish(chainId, rc.App(), profile)
			}

			logWebSocket(fmt.Sprintf("⏱️ Request processing time: %dms", endTime-startTime))
		}
	})

	if err != nil {
		logWebSocket(fmt.Sprintf("❌ Error WebSocket upgrade: %v", err))
		a.logger.Error().Err(err).Msg("WebSocket upgrade failed")
		return
	}
}

// checkOrigin verifies the origin of the WebSocket connection
func checkOrigin(r *fasthttp.RequestCtx) bool {
	return true // In production, origin check should be configured
}
