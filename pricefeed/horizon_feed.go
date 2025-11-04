package pricefeed

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/jacquesbecker/sdex-marketmaker/config"
	"github.com/jacquesbecker/sdex-marketmaker/offers"
)

// HorizonOrderBookResponse represents the order book response from Horizon API
type HorizonOrderBookResponse struct {
	Bids []HorizonOrderBookEntry `json:"bids"`
	Asks []HorizonOrderBookEntry `json:"asks"`
	Base HorizonAsset            `json:"base"`
	Counter HorizonAsset         `json:"counter"`
}

// HorizonOrderBookEntry represents a single order book entry
type HorizonOrderBookEntry struct {
	PriceR struct {
		N int `json:"n"`
		D int `json:"d"`
	} `json:"price_r"`
	Price  string `json:"price"`
	Amount string `json:"amount"`
}

// HorizonAsset represents an asset in the Horizon response
type HorizonAsset struct {
	AssetType   string `json:"asset_type"`
	AssetCode   string `json:"asset_code,omitempty"`
	AssetIssuer string `json:"asset_issuer,omitempty"`
}

// FatalError represents an unrecoverable error that should stop the program
type FatalError struct {
	msg string
}

func (e *FatalError) Error() string {
	return e.msg
}

// isFatalError checks if an error is fatal (should not retry)
func isFatalError(err error) bool {
	var fatalErr *FatalError
	return errors.As(err, &fatalErr)
}

// HorizonFeed manages the SSE connection to Horizon and streams order book data
type HorizonFeed struct {
	config              *config.BotConfig
	reconnect           bool
	horizonBaseURI      string
	httpClient          *http.Client
	Features            *FeaturesBuffer
	LastOrderbook       *OrderbookState
	FeatureBuilder      FeatureBuilder
	LastMessageAt       int64 // unix ms of last SSE message read
	obLogCounter        int
	mu                  sync.RWMutex
	offerMonitor        *offers.Monitor
	onFeatureUpdate     FeatureUpdateCallback
	onDisconnect        DisconnectCallback
	consecutiveFailures int // Track consecutive connection failures
}

// NewHorizonFeed creates a new Horizon price feed
func NewHorizonFeed(botConfig *config.BotConfig, horizonBaseURI string, offerMonitor *offers.Monitor) *HorizonFeed {
	// Get HTTP timeout from config (default to 60 seconds)
	timeoutSeconds := botConfig.PriceFeedOptions.SSETimeoutSeconds
	if timeoutSeconds == 0 {
		timeoutSeconds = 60
	}
	
	return &HorizonFeed{
		config:         botConfig,
		reconnect:      true,
		horizonBaseURI: horizonBaseURI,
		httpClient: &http.Client{
			Timeout: time.Duration(timeoutSeconds) * time.Second,
		},
		Features:       NewFeaturesBuffer(botConfig.PriceFeedOptions.BufferLength),
		FeatureBuilder: NewFeatureBuilder(botConfig.StrategyOptions.VolWindowMs),
		offerMonitor:   offerMonitor,
	}
}

// Start begins polling the order book
func (h *HorizonFeed) Start() error {
	log.Printf("Starting Horizon price feed (polling) for %s/%s", h.config.BaseAsset, h.config.CounterAsset)
	
	// Get tick rate from config (default to 3000ms)
	tickRate := h.config.PriceFeedOptions.OrderBookTickRate
	if tickRate == 0 {
		tickRate = 3000
	}
	log.Printf("[HORIZON POLL] Tick Rate: %dms | Horizon: %s", tickRate, h.horizonBaseURI)
	
	ticker := time.NewTicker(time.Duration(tickRate) * time.Millisecond)
	defer ticker.Stop()
	
	// Fetch immediately on start
	log.Println("[HORIZON POLL] Fetching initial order book...")
	if err := h.fetchOrderBook(); err != nil {
		log.Printf("[HORIZON POLL] Initial fetch error: %v", err)
	} else {
		log.Println("[HORIZON POLL] Initial order book fetched successfully")
	}
	
	// Start polling loop
	for h.reconnect {
		select {
		case <-ticker.C:
			if !h.reconnect {
				return nil
			}
			if err := h.fetchOrderBook(); err != nil {
				log.Printf("[HORIZON POLL] Fetch error: %v", err)
				// Invoke disconnect callback to clear strategy prices
				if h.onDisconnect != nil {
					h.onDisconnect()
				}
			}
		}
	}
	return nil
}

// fetchOrderBook fetches the order book via REST API
func (h *HorizonFeed) fetchOrderBook() error {
	// Build the order book URL
	url := h.buildOrderBookURL()

	resp, err := h.httpClient.Get(url)
	if err != nil {
		return fmt.Errorf("failed to fetch order book: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("horizon returned status %d", resp.StatusCode)
	}

	// Read and parse response
	var orderBook HorizonOrderBookResponse
	if err := json.NewDecoder(resp.Body).Decode(&orderBook); err != nil {
		return fmt.Errorf("failed to decode order book: %w", err)
	}
	
	// Update last message timestamp
	h.LastMessageAt = time.Now().UnixMilli()
	
	// Process order book
	h.processOrderBookData(&orderBook)
	
	return nil
}

// buildOrderBookURL constructs the Horizon order book SSE URL
func (h *HorizonFeed) buildOrderBookURL() string {
	// Determine asset parameters
	sellingAssetType := "native"
	sellingAssetCode := ""
	sellingAssetIssuer := ""
	
	buyingAssetType := "native"
	buyingAssetCode := ""
	buyingAssetIssuer := ""

	// Base asset (what we're trading)
	if h.config.BaseAssetIssuer != "native" {
		sellingAssetType = "credit_alphanum4"
		if len(h.config.BaseAsset) > 4 {
			sellingAssetType = "credit_alphanum12"
		}
		sellingAssetCode = h.config.BaseAsset
		sellingAssetIssuer = h.config.BaseAssetIssuer
	}

	// Counter asset (quote currency)
	if h.config.CounterAssetIssuer != "native" {
		buyingAssetType = "credit_alphanum4"
		if len(h.config.CounterAsset) > 4 {
			buyingAssetType = "credit_alphanum12"
		}
		buyingAssetCode = h.config.CounterAsset
		buyingAssetIssuer = h.config.CounterAssetIssuer
	}

	// Build URL with parameters
	url := fmt.Sprintf("%s/order_book?selling_asset_type=%s&buying_asset_type=%s",
		h.horizonBaseURI, sellingAssetType, buyingAssetType)

	if sellingAssetCode != "" {
		url += fmt.Sprintf("&selling_asset_code=%s&selling_asset_issuer=%s",
			sellingAssetCode, sellingAssetIssuer)
	}

	if buyingAssetCode != "" {
		url += fmt.Sprintf("&buying_asset_code=%s&buying_asset_issuer=%s",
			buyingAssetCode, buyingAssetIssuer)
	}

	// Limit depth
	limit := h.config.PriceFeedOptions.BookDepth
	if limit == 0 {
		limit = 20 // Default
	}
	url += fmt.Sprintf("&limit=%d", limit)

	return url
}

// processOrderBookData processes an order book response
func (h *HorizonFeed) processOrderBookData(orderBook *HorizonOrderBookResponse) {

	// Get current offers from offer monitor to filter them out
	currentOffers := h.offerMonitor.GetOffers()
	
	// DEBUG: Log raw orderbook sizes BEFORE filtering
	log.Printf("[DEBUG OB] RAW from Horizon: %d bids, %d asks", len(orderBook.Bids), len(orderBook.Asks))
	
	// Convert to OrderBookLevels and filter out our own offers
	// Horizon's convention matches ours: bids=buying XLM, asks=selling XLM
	bids := h.filterAndConvertLevels(orderBook.Bids, currentOffers, offers.OfferTypeBid)
	asks := h.filterAndConvertLevels(orderBook.Asks, currentOffers, offers.OfferTypeAsk)
	
	// DEBUG: Log after filtering
	log.Printf("[DEBUG OB] AFTER filtering: %d bids, %d asks", len(bids), len(asks))

	// Validate we have data
	if len(bids) == 0 || len(asks) == 0 {
		log.Printf("[HORIZON SSE] Empty order book after filtering (bids=%d, asks=%d)", len(bids), len(asks))
		return
	}

	// Convert to numeric OrderbookState
	utils := OrderbookUtils{}
	bestBid, bestAsk, bestBidQty, bestAskQty := utils.ComputeBest(bids, asks)
	depthBidSum, depthAskSum := utils.ComputeDepth(bids, asks, h.config.PriceFeedOptions.BookDepth)
	
	// DEBUG: Log computed depth values
	log.Printf("[DEBUG DEPTH] depthBidSum=%.2f, depthAskSum=%.2f (from %d levels)", 
		depthBidSum, depthAskSum, h.config.PriceFeedOptions.BookDepth)

	timestamp := time.Now().UnixMilli()
	obState := &OrderbookState{
		Timestamp:    timestamp,
		LastUpdateID: timestamp, // Use timestamp as update ID
		Bids:         bids,
		Asks:         asks,
		BestBid:      bestBid,
		BestAsk:      bestAsk,
		BestBidQty:   bestBidQty,
		BestAskQty:   bestAskQty,
		DepthBidSum:  depthBidSum,
		DepthAskSum:  depthAskSum,
	}
	h.LastOrderbook = obState

	// Build features
	featureRow, ok := h.FeatureBuilder.Build(obState, h.Features)
	if !ok {
		return
	}

	// Reset consecutive failures on first successful data
	if h.consecutiveFailures > 0 {
		log.Printf("[HORIZON SSE] Successfully receiving data, resetting failure counter")
		h.consecutiveFailures = 0
	}
	
	// Append feature
	h.Features.Append(featureRow)
	
	// Invoke callback if set
	if h.onFeatureUpdate != nil {
		h.onFeatureUpdate()
	}

	// Debug: log feature buffer status
	h.Features.mu.RLock()
	count := h.Features.count
	h.Features.mu.RUnlock()

	if count <= 5 {
		log.Printf("[FEATURES] Appended feature row. Buffer count: %d. RollingVol: %v", count, featureRow.RollingVolatility)
	}

	// Calculate mid price for logging
	midPrice := (bestBid + bestAsk) / 2.0

	// Log every order book update
	h.obLogCounter++
	if true {
		spread := bestAsk - bestBid
		spreadBps := (spread / midPrice) * 10000
		depthImb := (depthBidSum - depthAskSum) / (depthBidSum + depthAskSum)
		tobImb := (bestBidQty - bestAskQty) / (bestBidQty + bestAskQty)

		log.Printf("\n📊 [ORDER BOOK] Horizon %s/%s", h.config.BaseAsset, h.config.CounterAsset)
		log.Printf("   Mid: %.6f | Spread: %.6f (%.1f bps)", midPrice, spread, spreadBps)
		log.Printf("   L1: Bid %.6f@%.0f | Ask %.6f@%.0f", bestBid, bestBidQty, bestAsk, bestAskQty)
		log.Printf("   Depth(%d): Bids=%.0f Asks=%.0f | TOB: %.3f | Depth: %.3f",
			h.config.PriceFeedOptions.BookDepth, depthBidSum, depthAskSum, tobImb, depthImb)
		log.Printf("   Filtered out %d own offers\n", len(currentOffers))
	}
}

// filterAndConvertLevels converts Horizon entries to OrderBookLevels and filters out our own offers
func (h *HorizonFeed) filterAndConvertLevels(entries []HorizonOrderBookEntry, currentOffers map[string]*offers.Offer, offerType offers.OfferType) []OrderBookLevel {
	levels := []OrderBookLevel{}
	
	// Track which offers have been matched to prevent over-filtering
	matchedOffers := make(map[string]bool)

	for _, entry := range entries {
		// Parse price and amount
		price, err := strconv.ParseFloat(entry.Price, 64)
		if err != nil {
			continue
		}
		amount, err := strconv.ParseFloat(entry.Amount, 64)
		if err != nil {
			continue
		}

		// Check if this price matches any of our offers and subtract quantity
		remainingAmount := amount
		for offerID, offer := range currentOffers {
			// Skip if wrong type or already matched
			if offer.Type != offerType || matchedOffers[offerID] {
				continue
			}

			offerPrice, _ := strconv.ParseFloat(offer.Price, 64)
			offerQty, _ := strconv.ParseFloat(offer.Quantity, 64)

			// Check if price matches (within small tolerance)
			priceDiff := abs(price - offerPrice)

			// If price matches, subtract our quantity from this order book level
			if priceDiff < 0.000001 {
				remainingAmount -= offerQty
				matchedOffers[offerID] = true  // Mark this offer as matched
				// Don't break - there might be multiple offers at same price
			}
		}

		// Only include if there's remaining quantity after subtracting our offers
		if remainingAmount > 0.0001 { // Small threshold to avoid near-zero entries
			levels = append(levels, OrderBookLevel{
				Price:    entry.Price,
				Quantity: fmt.Sprintf("%.7f", remainingAmount),
			})
		}
	}

	return levels
}

// abs returns the absolute value of a float64
func abs(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}

// GetLatestFeatures returns the most recent feature row
func (h *HorizonFeed) GetLatestFeatures() (FeaturesRow, bool) {
	return h.Features.Latest()
}

// GetLastMessageAt returns the last time a SSE message was received (unix ms)
func (h *HorizonFeed) GetLastMessageAt() int64 {
	return h.LastMessageAt
}

// SetFeatureUpdateCallback sets the callback to invoke when features are updated
func (h *HorizonFeed) SetFeatureUpdateCallback(callback FeatureUpdateCallback) {
	h.onFeatureUpdate = callback
}

// SetDisconnectCallback sets the callback to invoke when feed disconnects
func (h *HorizonFeed) SetDisconnectCallback(callback DisconnectCallback) {
	h.onDisconnect = callback
}

// Stop gracefully stops the price feed
func (h *HorizonFeed) Stop() {
	log.Println("Stopping Horizon price feed...")
	h.reconnect = false
	// HTTP client will timeout/close on next read
}
