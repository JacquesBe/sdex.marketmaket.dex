package pricefeed

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"runtime/debug"
	"strconv"
	"strings"
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
	// Determine SSE endpoint - use official Stellar Horizon for SSE (more stable)
	sseBaseURI := horizonBaseURI
	if strings.Contains(horizonBaseURI, "validationcloud.io") {
		// ValidationCloud closes SSE connections frequently, use official Horizon for streaming
		if strings.Contains(horizonBaseURI, "testnet") {
			sseBaseURI = "https://horizon-testnet.stellar.org"
		} else {
			sseBaseURI = "https://horizon.stellar.org"
		}
		log.Printf("[HORIZON SSE] Using official Stellar Horizon for SSE: %s", sseBaseURI)
	}
	
	return &HorizonFeed{
		config:         botConfig,
		reconnect:      true,
		horizonBaseURI: sseBaseURI,
		httpClient: &http.Client{
			Timeout: 0, // No timeout for SSE streaming
		},
		Features:       NewFeaturesBuffer(botConfig.PriceFeedOptions.BufferLength),
		FeatureBuilder: NewFeatureBuilder(botConfig.StrategyOptions.VolWindowMs),
		offerMonitor:   offerMonitor,
	}
}

// Start begins the SSE connection and starts streaming order book data
func (h *HorizonFeed) Start() error {
	log.Printf("Starting Horizon price feed for %s/%s", h.config.BaseAsset, h.config.CounterAsset)

	for h.reconnect {
		if err := h.connectAndStream(); err != nil {
			// Check if this is a fatal error
			if isFatalError(err) {
				log.Printf("[HORIZON SSE] FATAL ERROR: %v", err)
				log.Printf("[HORIZON SSE] Stack trace:\n%s", debug.Stack())
				return fmt.Errorf("fatal Horizon feed error: %w", err)
			}
			
			// Non-fatal error - log and reconnect immediately
			log.Printf("[HORIZON SSE] Connection error: %v. Reconnecting immediately...", err)
			
			// Invoke disconnect callback to clear strategy prices
			if h.onDisconnect != nil {
				h.onDisconnect()
			}
			
			// Reconnect immediately (no delay, no failure limit)
			continue
		}
	}

	return nil
}

// connectAndStream establishes SSE connection and streams order book updates
func (h *HorizonFeed) connectAndStream() error {
	// Build the order book URL
	url := h.buildOrderBookURL()
	log.Printf("[HORIZON SSE] Connecting to: %s", url)

	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return fmt.Errorf("failed to create request: %w", err)
	}

	// Set headers for SSE
	req.Header.Set("Accept", "text/event-stream")

	resp, err := h.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to connect to Horizon SSE: %w", err)
	}
	defer resp.Body.Close()

	// HTTP errors that indicate configuration problems (fatal)
	if resp.StatusCode == http.StatusNotFound {
		return &FatalError{msg: fmt.Sprintf("Horizon endpoint not found (404) - check trading pair configuration")}
	}
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return &FatalError{msg: fmt.Sprintf("Horizon authentication failed (%d) - check API credentials", resp.StatusCode)}
	}
	if resp.StatusCode == http.StatusBadRequest {
		return &FatalError{msg: fmt.Sprintf("Horizon bad request (400) - check asset configuration")}
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("horizon returned status %d", resp.StatusCode)
	}

	log.Println("[HORIZON SSE] Successfully connected to Horizon order book stream")

	// Read SSE stream
	scanner := bufio.NewScanner(resp.Body)
	var eventData strings.Builder

	// Track last successful scan to detect silent disconnections
	lastScan := time.Now()
	
	// Get timeout from config (default to 60 seconds if not set)
	timeoutSeconds := h.config.PriceFeedOptions.SSETimeoutSeconds
	if timeoutSeconds == 0 {
		timeoutSeconds = 60
	}
	timeoutDuration := time.Duration(timeoutSeconds) * time.Second
	checkInterval := time.Duration(timeoutSeconds/6) * time.Second // Check every ~10s for 60s timeout
	if checkInterval < 2*time.Second {
		checkInterval = 2 * time.Second
	}
	
	// Monitor for read timeout in a separate goroutine
	readTimeout := make(chan bool, 1)
	go func() {
		for {
			time.Sleep(checkInterval)
			if time.Since(lastScan) > timeoutDuration {
				log.Printf("[HORIZON SSE] Read timeout: no data received for %v (timeout: %v)", time.Since(lastScan), timeoutDuration)
				readTimeout <- true
				return
			}
		}
	}()
	
	for {
		select {
		case <-readTimeout:
			// Timeout occurred - close connection and retry
			return fmt.Errorf("read timeout: no data received")
		default:
			// Try to scan next line
			if !scanner.Scan() {
				// Scanner stopped - check error or EOF
				goto scannerDone
			}
			lastScan = time.Now()
			line := scanner.Text()

			// SSE format: "data: {...json...}"
			if strings.HasPrefix(line, "data: ") {
				eventData.WriteString(strings.TrimPrefix(line, "data: "))
			} else if line == "" {
				// Empty line indicates end of event
				if eventData.Len() > 0 {
					h.LastMessageAt = time.Now().UnixMilli()
					h.processOrderBook(eventData.String())
					eventData.Reset()
				}
			}
		}
	}

	scannerDone:

	if err := scanner.Err(); err != nil {
		log.Printf("[HORIZON SSE] Scanner error: %v", err)
		return &FatalError{msg: fmt.Sprintf("SSE scanner error: %v", err)}
	}
	
	// If we exit the loop without error, the connection closed gracefully (not fatal - just reconnect)
	log.Printf("[HORIZON SSE] Connection closed by server (last scan was %v ago)", time.Since(lastScan))
	return fmt.Errorf("connection closed by server")
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

// processOrderBook processes an order book update from SSE
func (h *HorizonFeed) processOrderBook(data string) {
	// Skip "hello" and "byebye" messages from Horizon SSE
	if data == `"hello"` || strings.Contains(data, `"hello"`) {
		return
	}
	if data == `"byebye"` || strings.Contains(data, `"byebye"`) {
		log.Println("[HORIZON SSE] Received 'byebye' - server closing connection")
		return
	}
	
	var orderBook HorizonOrderBookResponse
	if err := json.Unmarshal([]byte(data), &orderBook); err != nil {
		log.Printf("[HORIZON SSE] Error parsing order book: %v | Data: %s", err, data)
		return
	}

	// Get current offers from offer monitor to filter them out
	currentOffers := h.offerMonitor.GetOffers()
	
	// Convert to OrderBookLevels and filter out our own offers
	bids := h.filterAndConvertLevels(orderBook.Bids, currentOffers, offers.OfferTypeBid)
	asks := h.filterAndConvertLevels(orderBook.Asks, currentOffers, offers.OfferTypeAsk)

	// Validate we have data
	if len(bids) == 0 || len(asks) == 0 {
		log.Printf("[HORIZON SSE] Empty order book after filtering (bids=%d, asks=%d)", len(bids), len(asks))
		return
	}

	// Convert to numeric OrderbookState
	utils := OrderbookUtils{}
	bestBid, bestAsk, bestBidQty, bestAskQty := utils.ComputeBest(bids, asks)
	depthBidSum, depthAskSum := utils.ComputeDepth(bids, asks, h.config.PriceFeedOptions.BookDepth)

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

		// Check if this price/amount matches any of our offers
		isOurOffer := false
		for offerID, offer := range currentOffers {
			// Skip if wrong type or already matched
			if offer.Type != offerType || matchedOffers[offerID] {
				continue
			}

			offerPrice, _ := strconv.ParseFloat(offer.Price, 64)
			offerQty, _ := strconv.ParseFloat(offer.Quantity, 64)

			// Check if price and quantity match (with small tolerance)
			priceDiff := abs(price - offerPrice)
			qtyDiff := abs(amount - offerQty)

			// If both price and quantity are very close, consider it our offer
			if priceDiff < 0.000001 && qtyDiff < 0.0001 {
				isOurOffer = true
				matchedOffers[offerID] = true  // Mark this offer as matched
				break
			}
		}

		// Only include if it's not our offer
		if !isOurOffer {
			levels = append(levels, OrderBookLevel{
				Price:    entry.Price,
				Quantity: entry.Amount,
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
