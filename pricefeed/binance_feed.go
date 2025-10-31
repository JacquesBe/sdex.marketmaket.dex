package pricefeed

import (
	"encoding/json"
	"fmt"
	"log"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/jacquesbecker/sdex-marketmaker/config"
)

// OrderBookLevel represents a single level in the order book
type OrderBookLevel struct {
	Price    string
	Quantity string
}

// BinanceDepthSnapshot represents the snapshot from partial book depth stream
type BinanceDepthSnapshot struct {
	LastUpdateID int64              `json:"lastUpdateId"` // Last update ID
	Bids         [][]string         `json:"bids"`         // Bids [price, quantity]
	Asks         [][]string         `json:"asks"`         // Asks [price, quantity]
}

// BinanceDepthMessage represents the partial book depth stream from Binance
type BinanceDepthMessage struct {
	EventType   string            `json:"e"` // Event type ("depthUpdate")
	EventTime   int64             `json:"E"` // Event time
	Symbol      string            `json:"s"` // Symbol
	FirstUpdate int64             `json:"U"` // First update ID
	LastUpdate  int64             `json:"u"` // Last update ID
	Bids        [][]string        `json:"b"` // Bids to be updated [price, quantity]
	Asks        [][]string        `json:"a"` // Asks to be updated [price, quantity]
}

// OrderBook represents the current state of the order book
type OrderBook struct {
	mu        sync.RWMutex
	Bids      []OrderBookLevel // Sorted by price descending (best bid first)
	Asks      []OrderBookLevel // Sorted by price ascending (best ask first)
	LastUpdate int64
}

// NewOrderBook creates a new empty order book
func NewOrderBook() *OrderBook {
	return &OrderBook{
		Bids: []OrderBookLevel{},
		Asks: []OrderBookLevel{},
	}
}

// Update updates the order book with new data
func (ob *OrderBook) Update(bids, asks []OrderBookLevel, updateID int64) {
	ob.mu.Lock()
	defer ob.mu.Unlock()
	
	ob.Bids = bids
	ob.Asks = asks
	ob.LastUpdate = updateID
}


// PriceData represents a single price point with timestamp
type PriceData struct {
	Timestamp int64
	BidPrice  float64
	AskPrice  float64
	MidPrice  float64
	LastPrice float64
}

// OrderbookState represents a numeric snapshot of the order book
type OrderbookState struct {
	Timestamp     int64
	LastUpdateID  int64
	Bids          []OrderBookLevel
	Asks          []OrderBookLevel
	BestBid       float64
	BestAsk       float64
	BestBidQty    float64
	BestAskQty    float64
	DepthBidSum   float64
	DepthAskSum   float64
}

// FeaturesRow represents a compact feature vector for strategy layer
type FeaturesRow struct {
	Ts                   int64
	ExternalMidPrice     float64
	MicroPrice           float64
	RollingVolatility    *float64 // nullable if not enough data
	BidPenalty           float64
	AskPenalty           float64
}

// PriceBuffer is a thread-safe circular buffer for price data
type PriceBuffer struct {
	mu     sync.RWMutex
	data   []PriceData
	size   int
	head   int
	count  int
}

// NewPriceBuffer creates a new circular buffer with the specified size
func NewPriceBuffer(size int) *PriceBuffer {
	if size <= 0 {
		size = 1000 // Default size
	}
	return &PriceBuffer{
		data: make([]PriceData, size),
		size: size,
	}
}

// Add adds a new price to the buffer (FIFO - oldest is removed when full)
func (pb *PriceBuffer) Add(price PriceData) {
	pb.mu.Lock()
	defer pb.mu.Unlock()
	
	pb.data[pb.head] = price
	pb.head = (pb.head + 1) % pb.size
	
	if pb.count < pb.size {
		pb.count++
	}
}

// OrderbookUtils provides helper functions for order book calculations
type OrderbookUtils struct{}

// ComputeBest extracts best bid/ask price and quantity from order book levels
func (OrderbookUtils) ComputeBest(bids, asks []OrderBookLevel) (bestBid, bestAsk, bestBidQty, bestAskQty float64) {
	if len(bids) > 0 {
		fmt.Sscanf(bids[0].Price, "%f", &bestBid)
		fmt.Sscanf(bids[0].Quantity, "%f", &bestBidQty)
	}
	if len(asks) > 0 {
		fmt.Sscanf(asks[0].Price, "%f", &bestAsk)
		fmt.Sscanf(asks[0].Quantity, "%f", &bestAskQty)
	}
	return
}

// ComputeDepth sums quantities across specified depth levels
func (OrderbookUtils) ComputeDepth(bids, asks []OrderBookLevel, depth int) (bidSum, askSum float64) {
	for i := 0; i < depth && i < len(bids); i++ {
		var qty float64
		fmt.Sscanf(bids[i].Quantity, "%f", &qty)
		bidSum += qty
	}
	for i := 0; i < depth && i < len(asks); i++ {
		var qty float64
		fmt.Sscanf(asks[i].Quantity, "%f", &qty)
		askSum += qty
	}
	return
}

// Helper functions
func max(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}

func sqrt(x float64) float64 {
	return math.Sqrt(x)
}

// FeatureBuilder builds feature rows from order book state and balances
type FeatureBuilder struct {
	volWindowMs int64
}

// NewFeatureBuilder creates a new feature builder with the given volatility window
func NewFeatureBuilder(volWindowMs int64) FeatureBuilder {
	return FeatureBuilder{volWindowMs: volWindowMs}
}

// Build constructs a FeaturesRow from order book state and historical features buffer
func (fb FeatureBuilder) Build(ob *OrderbookState, featuresBuffer *FeaturesBuffer) (FeaturesRow, bool) {
	if ob == nil {
		return FeaturesRow{}, false
	}
	
	// Calculate externalMidPrice: (best bid + best ask) / 2
	externalMidPrice := (ob.BestBid + ob.BestAsk) / 2.0
	
	// Calculate microPrice: ((best ask * size at best bid) + (best bid * size at best ask)) / (size at best bid + size at best ask)
	denomL1 := ob.BestBidQty + ob.BestAskQty
	var microPrice float64
	if denomL1 > 0 {
		microPrice = (ob.BestAsk*ob.BestBidQty + ob.BestBid*ob.BestAskQty) / denomL1
	} else {
		microPrice = externalMidPrice
	}
	
	// Calculate TOB: (size at best bid - size at best ask) / (size at best bid + size at best ask)
	var tob float64
	if denomL1 > 0 {
		tob = (ob.BestBidQty - ob.BestAskQty) / denomL1
	}
	
	// Calculate penalties
	bidPenalty := max(0, -tob)  // max(0, -TOB)
	askPenalty := max(0, tob)   // max(0, +TOB)
	
	// Calculate rolling volatility if we have enough data
	var rollingVolatility *float64
	if featuresBuffer != nil && fb.volWindowMs > 0 {
		window := featuresBuffer.WindowSince(fb.volWindowMs)
		if len(window) >= 2 {
			// Compute standard deviation of externalMidPrice over the window
			var sum, sumSq float64
			for _, row := range window {
				sum += row.ExternalMidPrice
				sumSq += row.ExternalMidPrice * row.ExternalMidPrice
			}
			n := float64(len(window))
			mean := sum / n
			variance := (sumSq / n) - (mean * mean)
			if variance > 0 {
				stdDev := sqrt(variance)
				rollingVolatility = &stdDev
			}
		}
	}
	
	return FeaturesRow{
		Ts:                ob.Timestamp,
		ExternalMidPrice:  externalMidPrice,
		MicroPrice:        microPrice,
		RollingVolatility: rollingVolatility,
		BidPenalty:        bidPenalty,
		AskPenalty:        askPenalty,
	}, true
}

// FeaturesBuffer is a thread-safe circular buffer for feature rows
type FeaturesBuffer struct {
	mu    sync.RWMutex
	data  []FeaturesRow
	size  int
	head  int
	count int
}

// NewFeaturesBuffer creates a new circular buffer for features
func NewFeaturesBuffer(size int) *FeaturesBuffer {
	if size <= 0 {
		size = 1000
	}
	return &FeaturesBuffer{
		data: make([]FeaturesRow, size),
		size: size,
	}
}

// Append adds a new feature row to the buffer (FIFO)
func (fb *FeaturesBuffer) Append(row FeaturesRow) {
	fb.mu.Lock()
	defer fb.mu.Unlock()
	
	fb.data[fb.head] = row
	fb.head = (fb.head + 1) % fb.size
	
	if fb.count < fb.size {
		fb.count++
	}
}

// Latest returns the most recent feature row
func (fb *FeaturesBuffer) Latest() (FeaturesRow, bool) {
	fb.mu.RLock()
	defer fb.mu.RUnlock()
	
	if fb.count == 0 {
		return FeaturesRow{}, false
	}
	
	idx := (fb.head - 1 + fb.size) % fb.size
	return fb.data[idx], true
}

// WindowSince returns all feature rows from last msBack milliseconds
func (fb *FeaturesBuffer) WindowSince(msBack int64) []FeaturesRow {
	fb.mu.RLock()
	defer fb.mu.RUnlock()
	
	if fb.count == 0 {
		return []FeaturesRow{}
	}
	
	// Get latest timestamp
	latestIdx := (fb.head - 1 + fb.size) % fb.size
	latestTs := fb.data[latestIdx].Ts
	cutoff := latestTs - msBack
	
	result := []FeaturesRow{}
	for i := 0; i < fb.count; i++ {
		idx := (fb.head - 1 - i + fb.size) % fb.size
		if fb.data[idx].Ts >= cutoff {
			result = append(result, fb.data[idx])
		} else {
			break
		}
	}
	
	return result
}

// BinanceFeed manages the WebSocket connection to Binance and streams price data
type BinanceFeed struct {
	config              *config.BotConfig
	conn                *websocket.Conn
	reconnect           bool
	wsEndpoint          string
	PriceBuffer         *PriceBuffer // Publicly accessible price buffer
	OrderBook           *OrderBook   // Publicly accessible order book
	Features            *FeaturesBuffer
	LastOrderbook       *OrderbookState
	LastAppend          int64
	MinAppendIntervalMs int64
	FeatureBuilder      FeatureBuilder
}

// NewBinanceFeed creates a new Binance price feed
func NewBinanceFeed(botConfig *config.BotConfig) *BinanceFeed {
	// Convert symbol to lowercase as Binance WebSocket requires lowercase
	symbol := strings.ToLower(botConfig.PriceFeedOptions.GetTradingSymbol())
	
	// Determine the appropriate depth level (Binance supports 5, 10, 20)
	depthLevel := 5
	if botConfig.PriceFeedOptions.BookDepth >= 20 {
		depthLevel = 20
	} else if botConfig.PriceFeedOptions.BookDepth >= 10 {
		depthLevel = 10
	}
	
	// Use Binance's partial book depth stream for order book data
	// Format: <symbol>@depth<levels>@100ms (updates every 100ms)
	wsEndpoint := fmt.Sprintf("wss://stream.binance.com:9443/ws/%s@depth%d@100ms", symbol, depthLevel)

	return &BinanceFeed{
		config:              botConfig,
		reconnect:           true,
		wsEndpoint:          wsEndpoint,
		PriceBuffer:         NewPriceBuffer(botConfig.PriceFeedOptions.BufferLength),
		OrderBook:           NewOrderBook(),
		Features:            NewFeaturesBuffer(botConfig.PriceFeedOptions.BufferLength),
		MinAppendIntervalMs: 100,
		FeatureBuilder:      NewFeatureBuilder(botConfig.StrategyOptions.VolWindowMs),
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

	msgCount := 0
	for {
		_, message, err := b.conn.ReadMessage()
		if err != nil {
			log.Printf("[BINANCE WS] Error reading message: %v", err)
			return
		}

		msgCount++
		if msgCount <= 3 {
			log.Printf("[BINANCE WS] Raw message #%d: %s", msgCount, string(message)[:min(200, len(message))])
		}

		// Try parsing as snapshot first
		var snapshot BinanceDepthSnapshot
		if err := json.Unmarshal(message, &snapshot); err == nil && snapshot.LastUpdateID > 0 {
			// Convert snapshot to depth message format
			depthMsg := &BinanceDepthMessage{
				EventTime:  time.Now().UnixMilli(),
				Symbol:     "XLMEUR",
				LastUpdate: snapshot.LastUpdateID,
				Bids:       snapshot.Bids,
				Asks:       snapshot.Asks,
			}
			
			if len(depthMsg.Bids) > 0 && len(depthMsg.Asks) > 0 {
				b.processOrderBook(depthMsg)
			}
			continue
		}

		// Try parsing as depth update
		var depthMsg BinanceDepthMessage
		if err := json.Unmarshal(message, &depthMsg); err != nil {
			log.Printf("[BINANCE WS] Error parsing message: %v | Raw: %s", err, string(message)[:min(100, len(message))])
			continue
		}

		// Validate message has data
		if len(depthMsg.Bids) == 0 || len(depthMsg.Asks) == 0 {
			if msgCount <= 3 {
				log.Printf("[BINANCE WS] Empty order book data (bids=%d asks=%d)", len(depthMsg.Bids), len(depthMsg.Asks))
			}
			continue
		}

		// Process the order book update
		b.processOrderBook(&depthMsg)
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// processOrderBook handles incoming order book depth updates
func (b *BinanceFeed) processOrderBook(depth *BinanceDepthMessage) {
	// Convert [][]string to []OrderBookLevel
	bids := make([]OrderBookLevel, len(depth.Bids))
	asks := make([]OrderBookLevel, len(depth.Asks))
	
	for i, bid := range depth.Bids {
		if len(bid) >= 2 {
			bids[i] = OrderBookLevel{Price: bid[0], Quantity: bid[1]}
		}
	}
	for i, ask := range depth.Asks {
		if len(ask) >= 2 {
			asks[i] = OrderBookLevel{Price: ask[0], Quantity: ask[1]}
		}
	}
	
	// Update the order book
	b.OrderBook.Update(bids, asks, depth.LastUpdate)

	// Convert to numeric OrderbookState
	utils := OrderbookUtils{}
	bestBid, bestAsk, bestBidQty, bestAskQty := utils.ComputeBest(bids, asks)
depthBidSum, depthAskSum := utils.ComputeDepth(bids, asks, b.config.PriceFeedOptions.BookDepth)
	
	obState := &OrderbookState{
		Timestamp:    depth.EventTime,
		LastUpdateID: depth.LastUpdate,
		Bids:         bids,
		Asks:         asks,
		BestBid:      bestBid,
		BestAsk:      bestAsk,
		BestBidQty:   bestBidQty,
		BestAskQty:   bestAskQty,
		DepthBidSum:  depthBidSum,
		DepthAskSum:  depthAskSum,
	}
	b.LastOrderbook = obState
	
	// Build features (pass the features buffer to compute rolling volatility)
	featureRow, ok := b.FeatureBuilder.Build(obState, b.Features)
	if !ok {
		return
	}
	
	// Throttle feature appends
	now := time.Now().UnixMilli()
	if now-b.LastAppend >= b.MinAppendIntervalMs {
		b.Features.Append(featureRow)
		b.LastAppend = now
		
		// Log every feature row calculation
		volStr := "null"
		if featureRow.RollingVolatility != nil {
			volStr = fmt.Sprintf("%.8f", *featureRow.RollingVolatility)
		}
		log.Printf("\n📈 [FEATURE ROW CALCULATED]")
		log.Printf("   ExternalMidPrice: %.6f | MicroPrice: %.6f", featureRow.ExternalMidPrice, featureRow.MicroPrice)
		log.Printf("   RollingVolatility: %s | BidPenalty: %.4f | AskPenalty: %.4f\n", volStr, featureRow.BidPenalty, featureRow.AskPenalty)
	}
	
	// Calculate mid price for legacy logging
	midPrice := (bestBid + bestAsk) / 2.0
	
	// Add to price buffer for legacy support
	priceData := PriceData{
		Timestamp: depth.EventTime,
		BidPrice:  bestBid,
		AskPrice:  bestAsk,
		MidPrice:  midPrice,
		LastPrice: midPrice,
	}
	b.PriceBuffer.Add(priceData)

	// Simplified logging - only log every 10th update to reduce noise
	static := struct {
		mu      sync.Mutex
		counter int
	}{}
	static.mu.Lock()
	static.counter++
	shouldLog := static.counter%10 == 0
	static.mu.Unlock()
	
	if shouldLog {
		spread := bestAsk - bestBid
		spreadBps := (spread / midPrice) * 10000
		depthImb := (depthBidSum - depthAskSum) / (depthBidSum + depthAskSum)
		tobImb := (bestBidQty - bestAskQty) / (bestBidQty + bestAskQty)
		
		log.Printf("\n📊 [ORDER BOOK] %s", depth.Symbol)
		log.Printf("   Mid: %.6f | Spread: %.6f (%.1f bps)", midPrice, spread, spreadBps)
		log.Printf("   L1: Bid %.6f@%.0f | Ask %.6f@%.0f", bestBid, bestBidQty, bestAsk, bestAskQty)
		log.Printf("   Depth(%d): Bids=%.0f Asks=%.0f | TOB: %.3f | Depth: %.3f\n",
			b.config.PriceFeedOptions.BookDepth, depthBidSum, depthAskSum, tobImb, depthImb)
	}
}

// GetLatestFeatures returns the most recent feature row
func (b *BinanceFeed) GetLatestFeatures() (FeaturesRow, bool) {
	return b.Features.Latest()
}

// Stop gracefully stops the price feed
func (b *BinanceFeed) Stop() {
	log.Println("Stopping Binance price feed...")
	b.reconnect = false
	if b.conn != nil {
		b.conn.Close()
	}
}
