package pricefeed

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/gorilla/websocket"
	"github.com/jacquesbecker/sdex-marketmaker/config"
)

// BinanceTickerMessage represents the ticker data from Binance WebSocket
// We use the 24hr ticker stream which provides comprehensive market data
type BinanceTickerMessage struct {
	EventType        string      `json:"e"` // Event type
	EventTime        int64       `json:"E"` // Event time
	Symbol           string      `json:"s"` // Symbol
	BidPrice         json.Number `json:"b"` // Best bid price
	BidQty           json.Number `json:"B"` // Best bid quantity
	AskPrice         json.Number `json:"a"` // Best ask price
	AskQty           json.Number `json:"A"` // Best ask quantity
	LastPrice        json.Number `json:"c"` // Last price
	WeightedAvgPrice json.Number `json:"w"` // Weighted average price
}

// BinanceFeed manages the WebSocket connection to Binance and streams price data
type BinanceFeed struct {
	config     *config.BotConfig
	conn       *websocket.Conn
	reconnect  bool
	wsEndpoint string
}

// NewBinanceFeed creates a new Binance price feed
func NewBinanceFeed(botConfig *config.BotConfig) *BinanceFeed {
	// Convert symbol to lowercase as Binance WebSocket requires lowercase
	symbol := strings.ToLower(botConfig.PriceFeedOptions.GetTradingSymbol())
	
	// Use Binance's 24hr ticker stream for real-time price updates
	wsEndpoint := fmt.Sprintf("wss://stream.binance.com:9443/ws/%s@ticker", symbol)

	return &BinanceFeed{
		config:     botConfig,
		reconnect:  true,
		wsEndpoint: wsEndpoint,
	}
}

// Start begins the WebSocket connection and starts streaming prices
// This runs as a background service and will auto-reconnect on disconnections
func (b *BinanceFeed) Start() error {
	log.Printf("Starting Binance price feed for %s", b.config.PriceFeedOptions.GetTradingSymbol())

	for b.reconnect {
		if err := b.connect(); err != nil {
			log.Printf("Connection error: %v. Reconnecting in 5 seconds...", err)
			time.Sleep(5 * time.Second)
			continue
		}

		// Listen for messages
		b.readMessages()

		// If we get here, connection was closed
		if b.reconnect {
			log.Println("Connection closed. Reconnecting in 5 seconds...")
			time.Sleep(5 * time.Second)
		}
	}

	return nil
}

// connect establishes the WebSocket connection to Binance
func (b *BinanceFeed) connect() error {
	var err error
	b.conn, _, err = websocket.DefaultDialer.Dial(b.wsEndpoint, nil)
	if err != nil {
		return fmt.Errorf("failed to connect to Binance WebSocket: %w", err)
	}

	log.Println("Successfully connected to Binance WebSocket")
	return nil
}

// readMessages continuously reads and processes messages from the WebSocket
func (b *BinanceFeed) readMessages() {
	defer b.conn.Close()

	for {
		_, message, err := b.conn.ReadMessage()
		if err != nil {
			log.Printf("Error reading message: %v", err)
			return
		}

		// Parse the ticker message
		var ticker BinanceTickerMessage
		decoder := json.NewDecoder(strings.NewReader(string(message)))
		decoder.UseNumber()
		if err := decoder.Decode(&ticker); err != nil {
			log.Printf("Error parsing message: %v", err)
			continue
		}

		// Process and log the price data
		b.processPrice(&ticker)
	}
}

// processPrice handles incoming price data and logs it
// In future, this will invoke the Strategy layer
func (b *BinanceFeed) processPrice(ticker *BinanceTickerMessage) {
	// Apply the price feed multiple if configured
	multiple := b.config.PriceFeedOptions.PriceFeedMultiple

	log.Printf("[PRICE FEED] %s", ticker.Symbol)
	log.Printf("  Event Type: %s | Event Time: %d", ticker.EventType, ticker.EventTime)
	log.Printf("  Bid: %s (Qty: %s)", ticker.BidPrice, ticker.BidQty)
	log.Printf("  Ask: %s (Qty: %s)", ticker.AskPrice, ticker.AskQty)
	log.Printf("  Last Price: %s", ticker.LastPrice)
	log.Printf("  Weighted Avg Price: %s", ticker.WeightedAvgPrice)
	bidPrice, _ := ticker.BidPrice.Float64()
	askPrice, _ := ticker.AskPrice.Float64()
	midPrice := (bidPrice + askPrice) / 2.0
	log.Printf("  Mid Price: %.7f", midPrice)
	log.Printf("  Price Multiple: %.2f", multiple)

	// TODO: In next phase, invoke Strategy layer with this price data
	// strategy.OnPriceUpdate(ticker)
}

// Stop gracefully stops the price feed
func (b *BinanceFeed) Stop() {
	log.Println("Stopping Binance price feed...")
	b.reconnect = false
	if b.conn != nil {
		b.conn.Close()
	}
}
