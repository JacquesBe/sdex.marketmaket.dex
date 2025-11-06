package pricefeed

import (
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/jacquesbecker/sdex-marketmaker/config"
)

// KrakenSubscribeMessage represents the subscription message for Kraken WS
type KrakenSubscribeMessage struct {
	Event        string                 `json:"event"`
	Pair         []string               `json:"pair,omitempty"`
	Subscription map[string]interface{} `json:"subscription,omitempty"`
}

// KrakenSpreadMessage represents spread updates from Kraken
type KrakenSpreadMessage []interface{} // [channelID, [bid, ask, timestamp, bidVolume, askVolume], channelName, pair]

// KrakenFeed manages dual WebSocket connections to Kraken for synthetic pair pricing
type KrakenFeed struct {
	config              *config.BotConfig
	baseConn            *websocket.Conn
	counterConn         *websocket.Conn
	reconnect           bool
	Features            *FeaturesBuffer
	LastOrderbook       *OrderbookState
	FeatureBuilder      FeatureBuilder
	LastMessageAt       int64
	onFeatureUpdate     FeatureUpdateCallback
	onDisconnect        DisconnectCallback
	mu                  sync.RWMutex
	
	// Price data for base asset (e.g., XLM/USD)
	baseBid       float64
	baseAsk       float64
	baseUpdatedAt int64
	
	// Price data for counter asset (e.g., SHX/USD)
	counterBid       float64
	counterAsk       float64
	counterUpdatedAt int64
	
	// Stellar orderbook imbalance (injected from external monitor)
	stellarBidPenalty float64
	stellarAskPenalty float64
	hasStellarOB      bool
}

// NewKrakenFeed creates a new Kraken price feed for synthetic pairs
func NewKrakenFeed(botConfig *config.BotConfig) *KrakenFeed {
	return &KrakenFeed{
		config:         botConfig,
		reconnect:      true,
		Features:       NewFeaturesBuffer(botConfig.PriceFeedOptions.BufferLength),
		FeatureBuilder: NewFeatureBuilder(botConfig.StrategyOptions.VolWindowMs),
	}
}

// Start begins the dual WebSocket connections and streams synthetic pair prices
func (k *KrakenFeed) Start() error {
	log.Printf("[KRAKEN] Starting dual price feed: %s/USD and %s/USD", 
		k.config.BaseAsset, k.config.CounterAsset)
	
	var wg sync.WaitGroup
	
	// Start base asset feed (e.g., XLM/USD)
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			k.mu.RLock()
			shouldReconnect := k.reconnect
			k.mu.RUnlock()
			
			if !shouldReconnect {
				log.Println("[KRAKEN BASE] Stopped")
				return
			}
			
			if err := k.connectAndStream(k.config.BaseAsset, "base"); err != nil {
				log.Printf("[KRAKEN BASE] Connection error: %v. Reconnecting in 5s...", err)
				time.Sleep(5 * time.Second)
			}
		}
	}()
	
	// Start counter asset feed (e.g., SHX/USD)
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			k.mu.RLock()
			shouldReconnect := k.reconnect
			k.mu.RUnlock()
			
			if !shouldReconnect {
				log.Println("[KRAKEN COUNTER] Stopped")
				return
			}
			
			if err := k.connectAndStream(k.config.CounterAsset, "counter"); err != nil {
				log.Printf("[KRAKEN COUNTER] Connection error: %v. Reconnecting in 5s...", err)
				time.Sleep(5 * time.Second)
			}
		}
	}()
	
	wg.Wait()
	return nil
}

// connectAndStream establishes WS connection for a single asset pair
func (k *KrakenFeed) connectAndStream(asset string, streamType string) error {
	// Kraken WS endpoint
	wsEndpoint := "wss://ws.kraken.com"
	
	conn, _, err := websocket.DefaultDialer.Dial(wsEndpoint, nil)
	if err != nil {
		return fmt.Errorf("failed to connect to Kraken: %w", err)
	}
	defer conn.Close()
	
	// Store connection reference
	k.mu.Lock()
	if streamType == "base" {
		k.baseConn = conn
	} else {
		k.counterConn = conn
	}
	k.mu.Unlock()
	
	// Build Kraken pair symbol with slash (e.g., "XLM/USD", "SHX/USD")
	// Kraken WebSocket API requires slash format, not concatenated
	krakenPair := strings.ToUpper(asset) + "/USD"
	log.Printf("[KRAKEN %s] Connecting to %s (asset: %s)...", strings.ToUpper(streamType), krakenPair, asset)
	
	// Subscribe to spread channel
	subscribeMsg := KrakenSubscribeMessage{
		Event: "subscribe",
		Pair:  []string{krakenPair},
		Subscription: map[string]interface{}{
			"name": "spread",
		},
	}
	
	if err := conn.WriteJSON(subscribeMsg); err != nil {
		return fmt.Errorf("failed to subscribe: %w", err)
	}
	
	log.Printf("[KRAKEN %s] Subscribed to %s spread", strings.ToUpper(streamType), krakenPair)
	
	// Read messages loop
	for {
		// Check if we should stop
		k.mu.RLock()
		shouldContinue := k.reconnect
		k.mu.RUnlock()
		
		if !shouldContinue {
			return nil // Clean exit
		}
		
		_, message, err := conn.ReadMessage()
		if err != nil {
			return fmt.Errorf("read message failed: %w", err)
		}
		
		k.LastMessageAt = time.Now().UnixMilli()
		
		// Try parsing as event message first (subscription confirmation, etc.)
		var eventMsg map[string]interface{}
		if err := json.Unmarshal(message, &eventMsg); err == nil {
			if event, ok := eventMsg["event"].(string); ok {
				log.Printf("[KRAKEN %s] Event: %s | Full message: %+v", strings.ToUpper(streamType), event, eventMsg)
				if event == "subscriptionStatus" {
					if status, ok := eventMsg["status"].(string); ok {
						if status == "subscribed" {
							log.Printf("[KRAKEN %s] ✅ Successfully subscribed", strings.ToUpper(streamType))
						} else if status == "error" {
							log.Printf("[KRAKEN %s] ❌ Subscription error: %+v", strings.ToUpper(streamType), eventMsg)
							return fmt.Errorf("subscription failed: %v", eventMsg)
						}
					}
				}
				continue
			}
		}
		
		// Try parsing as spread message
		var spreadMsg []interface{}
		if err := json.Unmarshal(message, &spreadMsg); err == nil && len(spreadMsg) >= 4 {
			// Spread message format: [channelID, [bid, ask, timestamp, bidVol, askVol], "spread", "PAIR"]
			if len(spreadMsg) >= 2 {
				if spreadData, ok := spreadMsg[1].([]interface{}); ok && len(spreadData) >= 2 {
					bidStr, _ := spreadData[0].(string)
					askStr, _ := spreadData[1].(string)
					
					var bid, ask float64
					fmt.Sscanf(bidStr, "%f", &bid)
					fmt.Sscanf(askStr, "%f", &ask)
					
					k.updatePrice(streamType, bid, ask)
				}
			}
		}
	}
}

// updatePrice updates the price for either base or counter asset and triggers synthetic pair computation
func (k *KrakenFeed) updatePrice(streamType string, bid, ask float64) {
	k.mu.Lock()
	now := time.Now().UnixMilli()
	
	if streamType == "base" {
		k.baseBid = bid
		k.baseAsk = ask
		k.baseUpdatedAt = now
	} else {
		k.counterBid = bid
		k.counterAsk = ask
		k.counterUpdatedAt = now
	}
	k.mu.Unlock()
	
	// Compute synthetic pair (XLM/SHX) from XLM/USD and SHX/USD
	k.computeSyntheticPair()
}

// computeSyntheticPair calculates the synthetic pair price from two USD pairs
func (k *KrakenFeed) computeSyntheticPair() {
	k.mu.RLock()
	baseBid := k.baseBid
	baseAsk := k.baseAsk
	counterBid := k.counterBid
	counterAsk := k.counterAsk
	stellarBidPenalty := k.stellarBidPenalty
	stellarAskPenalty := k.stellarAskPenalty
	hasStellarOB := k.hasStellarOB
	k.mu.RUnlock()
	
	// Need both feeds to have data (at least once)
	if baseBid == 0 || baseAsk == 0 || counterBid == 0 || counterAsk == 0 {
		return
	}
	
	// Note: We don't check staleness - for illiquid pairs, we use the last known price
	// This allows XLM/USD updates to recalculate even if SHX/USD hasn't updated recently
	now := time.Now().UnixMilli()
	
	// Compute synthetic pair: XLM/SHX = (XLM/USD) / (SHX/USD)
	// midXLM = (baseBid + baseAsk) / 2
	// midSHX = (counterBid + counterAsk) / 2
	// syntheticMid = midXLM / midSHX
	
	midBase := (baseBid + baseAsk) / 2.0
	midCounter := (counterBid + counterAsk) / 2.0
	
	if midCounter == 0 {
		log.Printf("[KRAKEN] Counter mid price is zero, cannot compute synthetic pair")
		return
	}
	
	syntheticMid := midBase / midCounter
	
	// For bid/ask spread, we use cross-rate formulas:
	// To buy XLM (sell SHX): we pay baseAsk (buy XLM with USD) / counterBid (sell SHX for USD)
	// To sell XLM (buy SHX): we get baseBid (sell XLM for USD) / counterAsk (buy SHX with USD)
	
	syntheticBid := baseBid / counterAsk
	syntheticAsk := baseAsk / counterBid
	
	// Estimate quantities (L1) - simplified since we don't have depth from spread channel
	// Use equal quantities as placeholder
	syntheticBidQty := 100.0
	syntheticAskQty := 100.0
	
	// Build orderbook state
	obState := &OrderbookState{
		Timestamp:    now,
		LastUpdateID: 0,
		Bids:         []OrderBookLevel{},
		Asks:         []OrderBookLevel{},
		BestBid:      syntheticBid,
		BestAsk:      syntheticAsk,
		BestBidQty:   syntheticBidQty,
		BestAskQty:   syntheticAskQty,
		DepthBidSum:  syntheticBidQty,  // L1 only
		DepthAskSum:  syntheticAskQty,
	}
	k.LastOrderbook = obState
	
	// Build features
	featureRow, ok := k.FeatureBuilder.Build(obState, k.Features)
	if !ok {
		return
	}
	
	// Override OB imbalance with Stellar data if available
	if hasStellarOB {
		featureRow.BidPenalty = stellarBidPenalty
		featureRow.AskPenalty = stellarAskPenalty
	}
	
	// Append features
	k.Features.Append(featureRow)
	
	// Log every 50 updates to avoid spam
	k.Features.mu.RLock()
	count := k.Features.count
	k.Features.mu.RUnlock()
	
	if count%50 == 0 || count <= 5 {
		log.Printf("[KRAKEN SYNTHETIC] %s/%s = %.6f | Bid: %.6f | Ask: %.6f | Spread: %.1f bps | Vol: %v | Stellar OB: %v",
			k.config.BaseAsset, k.config.CounterAsset,
			syntheticMid, syntheticBid, syntheticAsk,
			((syntheticAsk-syntheticBid)/syntheticMid)*10000,
			featureRow.RollingVolatility,
			hasStellarOB)
	}
	
	// Invoke callback
	if k.onFeatureUpdate != nil {
		k.onFeatureUpdate()
	}
}

// SetStellarOrderbookImbalance allows external Stellar monitor to inject OB imbalance data
func (k *KrakenFeed) SetStellarOrderbookImbalance(bidPenalty, askPenalty float64) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.stellarBidPenalty = bidPenalty
	k.stellarAskPenalty = askPenalty
	k.hasStellarOB = true
}

// Stop gracefully stops the Kraken feed
func (k *KrakenFeed) Stop() {
	log.Println("[KRAKEN] Stopping...")
	
	k.mu.Lock()
	k.reconnect = false
	
	if k.baseConn != nil {
		k.baseConn.Close()
	}
	if k.counterConn != nil {
		k.counterConn.Close()
	}
	k.mu.Unlock()
	
	log.Println("[KRAKEN] Stopped")
}

// GetLatestFeatures returns the most recent feature row
func (k *KrakenFeed) GetLatestFeatures() (FeaturesRow, bool) {
	return k.Features.Latest()
}

// GetLastMessageAt returns the last time a message was received
func (k *KrakenFeed) GetLastMessageAt() int64 {
	return k.LastMessageAt
}

// SetFeatureUpdateCallback sets the callback for feature updates
func (k *KrakenFeed) SetFeatureUpdateCallback(callback FeatureUpdateCallback) {
	k.onFeatureUpdate = callback
}

// SetDisconnectCallback sets the callback for disconnections
func (k *KrakenFeed) SetDisconnectCallback(callback DisconnectCallback) {
	k.onDisconnect = callback
}
