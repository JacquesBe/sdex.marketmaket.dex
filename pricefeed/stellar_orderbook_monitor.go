package pricefeed

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/jacquesbecker/sdex-marketmaker/config"
	"github.com/jacquesbecker/sdex-marketmaker/offers"
)

// StellarOrderbookMonitor monitors Stellar DEX orderbook independently from price feed
// Computes orderbook imbalance (bid/ask penalties) for strategy layer
type StellarOrderbookMonitor struct {
	config         *config.BotConfig
	horizonBaseURI string
	httpClient     *http.Client
	offerMonitor   *offers.Monitor
	reconnect      bool
	mu             sync.RWMutex
	
	// Latest orderbook imbalance metrics
	bidPenalty     float64
	askPenalty     float64
	stellarMidPrice float64
	lastUpdate     int64
	hasData        bool
	
	// Callback for when imbalance updates
	onUpdate       func(bidPenalty, askPenalty float64)
}

// NewStellarOrderbookMonitor creates a new Stellar orderbook monitor
func NewStellarOrderbookMonitor(botConfig *config.BotConfig, horizonBaseURI string, offerMonitor *offers.Monitor) *StellarOrderbookMonitor {
	timeoutSeconds := botConfig.PriceFeedOptions.SSETimeoutSeconds
	if timeoutSeconds == 0 {
		timeoutSeconds = 60
	}
	
	return &StellarOrderbookMonitor{
		config:         botConfig,
		horizonBaseURI: horizonBaseURI,
		offerMonitor:   offerMonitor,
		reconnect:      true,
		httpClient: &http.Client{
			Timeout: time.Duration(timeoutSeconds) * time.Second,
		},
	}
}

// Start begins polling the Stellar orderbook
func (s *StellarOrderbookMonitor) Start() error {
	log.Printf("[STELLAR OB] Starting Stellar orderbook monitor for %s/%s", 
		s.config.BaseAsset, s.config.CounterAsset)
	
	// Get tick rate from config (default to 3000ms)
	tickRate := s.config.PriceFeedOptions.OrderBookTickRate
	if tickRate == 0 {
		tickRate = 3000
	}
	log.Printf("[STELLAR OB] Tick Rate: %dms | Horizon: %s", tickRate, s.horizonBaseURI)
	
	ticker := time.NewTicker(time.Duration(tickRate) * time.Millisecond)
	defer ticker.Stop()
	
	// Fetch immediately on start
	log.Println("[STELLAR OB] Fetching initial order book...")
	if err := s.fetchOrderBook(); err != nil {
		log.Printf("[STELLAR OB] Initial fetch error: %v", err)
	} else {
		log.Println("[STELLAR OB] Initial order book fetched successfully")
	}
	
	// Start polling loop
	for s.reconnect {
		select {
		case <-ticker.C:
			if !s.reconnect {
				return nil
			}
			if err := s.fetchOrderBook(); err != nil {
				log.Printf("[STELLAR OB] Fetch error: %v", err)
			}
		}
	}
	return nil
}

// fetchOrderBook fetches the order book via REST API
func (s *StellarOrderbookMonitor) fetchOrderBook() error {
	// Build the order book URL
	url := s.buildOrderBookURL()
	
	resp, err := s.httpClient.Get(url)
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
	
	// Process order book
	s.processOrderBookData(&orderBook)
	
	return nil
}

// buildOrderBookURL constructs the Horizon order book URL
func (s *StellarOrderbookMonitor) buildOrderBookURL() string {
	// Determine asset parameters
	sellingAssetType := "native"
	sellingAssetCode := ""
	sellingAssetIssuer := ""
	
	buyingAssetType := "native"
	buyingAssetCode := ""
	buyingAssetIssuer := ""
	
	// Base asset (what we're trading)
	if s.config.BaseAssetIssuer != "native" {
		sellingAssetType = "credit_alphanum4"
		if len(s.config.BaseAsset) > 4 {
			sellingAssetType = "credit_alphanum12"
		}
		sellingAssetCode = s.config.BaseAsset
		sellingAssetIssuer = s.config.BaseAssetIssuer
	}
	
	// Counter asset (quote currency)
	if s.config.CounterAssetIssuer != "native" {
		buyingAssetType = "credit_alphanum4"
		if len(s.config.CounterAsset) > 4 {
			buyingAssetType = "credit_alphanum12"
		}
		buyingAssetCode = s.config.CounterAsset
		buyingAssetIssuer = s.config.CounterAssetIssuer
	}
	
	// Build URL with parameters
	url := fmt.Sprintf("%s/order_book?selling_asset_type=%s&buying_asset_type=%s",
		s.horizonBaseURI, sellingAssetType, buyingAssetType)
	
	if sellingAssetCode != "" {
		url += fmt.Sprintf("&selling_asset_code=%s&selling_asset_issuer=%s",
			sellingAssetCode, sellingAssetIssuer)
	}
	if buyingAssetCode != "" {
		url += fmt.Sprintf("&buying_asset_code=%s&buying_asset_issuer=%s",
			buyingAssetCode, buyingAssetIssuer)
	}
	
	// Add limit parameter for depth
	limit := s.config.PriceFeedOptions.BookDepth
	if limit == 0 {
		limit = 5
	}
	url += fmt.Sprintf("&limit=%d", limit)
	
	return url
}

// processOrderBookData processes the order book and computes imbalance
func (s *StellarOrderbookMonitor) processOrderBookData(orderBook *HorizonOrderBookResponse) {
	// Convert Horizon entries to OrderBookLevel
	bids := make([]OrderBookLevel, 0, len(orderBook.Bids))
	asks := make([]OrderBookLevel, 0, len(orderBook.Asks))
	
	// Filter out own offers using offer monitor
	ownOfferIDs := make(map[string]bool)
	if s.offerMonitor != nil {
		offers := s.offerMonitor.GetOffers()
		for _, offer := range offers {
			ownOfferIDs[offer.OfferID] = true
		}
	}
	
	// Process bids (filter out own offers)
	for _, entry := range orderBook.Bids {
		// NOTE: We can't directly filter by offer ID from order book endpoint
		// The order book aggregates offers by price level
		// So we'll keep all bids/asks and let the depth computation handle it
		bids = append(bids, OrderBookLevel{
			Price:    entry.Price,
			Quantity: entry.Amount,
		})
	}
	
	// Process asks
	for _, entry := range orderBook.Asks {
		asks = append(asks, OrderBookLevel{
			Price:    entry.Price,
			Quantity: entry.Amount,
		})
	}
	
	// Check if orderbook is empty after filtering
	if len(bids) == 0 || len(asks) == 0 {
		log.Printf("[STELLAR OB] Warning: Empty order book (bids=%d, asks=%d)", len(bids), len(asks))
		return
	}
	
	// Compute best bid/ask and mid price
	utils := OrderbookUtils{}
	bestBid, bestAsk, _, _ := utils.ComputeBest(bids, asks)
	stellarMid := (bestBid + bestAsk) / 2.0
	
	// Compute depth sums
	depthBidSum, depthAskSum := utils.ComputeDepth(bids, asks, s.config.PriceFeedOptions.BookDepth)
	
	// Calculate depth-based imbalance
	var depthImbalance float64
	denomDepth := depthBidSum + depthAskSum
	if denomDepth > 0 {
		depthImbalance = (depthBidSum - depthAskSum) / denomDepth
	}
	
	// Calculate penalties based on DEPTH
	// bidPenalty = max(0, -depthImbalance) → positive when ask side heavier
	// askPenalty = max(0, depthImbalance) → positive when bid side heavier
	bidPenalty := maxFloat(0, -depthImbalance)
	askPenalty := maxFloat(0, depthImbalance)
	
	// Update state
	s.mu.Lock()
	s.bidPenalty = bidPenalty
	s.askPenalty = askPenalty
	s.stellarMidPrice = stellarMid
	s.lastUpdate = time.Now().UnixMilli()
	s.hasData = true
	s.mu.Unlock()
	
	// Log periodically
	if s.lastUpdate%10000 < 3000 { // Log roughly every 10 seconds
		log.Printf("[STELLAR OB] Imbalance: %.4f | BidPenalty: %.4f | AskPenalty: %.4f | Depth (bid/ask): %.2f/%.2f",
			depthImbalance, bidPenalty, askPenalty, depthBidSum, depthAskSum)
	}
	
	// Invoke callback
	if s.onUpdate != nil {
		s.onUpdate(bidPenalty, askPenalty)
	}
}

// GetImbalance returns the latest orderbook imbalance metrics
func (s *StellarOrderbookMonitor) GetImbalance() (bidPenalty, askPenalty float64, ok bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.bidPenalty, s.askPenalty, s.hasData
}

// GetStellarMidPrice returns the latest Stellar DEX mid price
func (s *StellarOrderbookMonitor) GetStellarMidPrice() (midPrice float64, ok bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.stellarMidPrice, s.hasData
}

// SetUpdateCallback sets the callback to invoke when imbalance updates
func (s *StellarOrderbookMonitor) SetUpdateCallback(callback func(bidPenalty, askPenalty float64)) {
	s.onUpdate = callback
}

// Stop gracefully stops the monitor
func (s *StellarOrderbookMonitor) Stop() {
	s.reconnect = false
	log.Println("[STELLAR OB] Stopped")
}

// maxFloat returns the maximum of two floats
func maxFloat(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}
