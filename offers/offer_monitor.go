package offers

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/jacquesbecker/sdex-marketmaker/config"
)

// HorizonOffer represents an offer from Horizon API
type HorizonOffer struct {
	ID               string `json:"id"`
	Seller           string `json:"seller"`
	Selling          Asset  `json:"selling"`
	Buying           Asset  `json:"buying"`
	Amount           string `json:"amount"`
	PriceR           Price  `json:"price_r"`
	Price            string `json:"price"`
	LastModifiedTime string `json:"last_modified_time"`
}

// Asset represents a Stellar asset
type Asset struct {
	AssetType   string `json:"asset_type"`
	AssetCode   string `json:"asset_code,omitempty"`
	AssetIssuer string `json:"asset_issuer,omitempty"`
}

// Price represents the rational price representation
type Price struct {
	N int `json:"n"` // numerator
	D int `json:"d"` // denominator
}

// HorizonOffersResponse represents the offers response from Horizon API
type HorizonOffersResponse struct {
	Embedded struct {
		Records []HorizonOffer `json:"records"`
	} `json:"_embedded"`
}

// Monitor monitors offers on the Stellar DEX
type Monitor struct {
	config         *config.BotConfig
	horizonBaseURI string
	httpClient     *http.Client
	stopChan       chan bool
	mu             sync.RWMutex
	offers         map[string]*Offer // offerID -> Offer
}

var (
	monitorInstance *Monitor
	monitorOnce     sync.Once
)

// NewMonitor creates a new offer monitor (singleton)
func NewMonitor(botConfig *config.BotConfig, horizonBaseURI string) *Monitor {
	monitorOnce.Do(func() {
		monitorInstance = &Monitor{
			config:         botConfig,
			horizonBaseURI: horizonBaseURI,
			httpClient: &http.Client{
				Timeout: 10 * time.Second,
			},
			stopChan: make(chan bool),
			offers:   make(map[string]*Offer),
		}
	})
	return monitorInstance
}

// GetMonitorInstance returns the singleton instance of the offer monitor
func GetMonitorInstance() *Monitor {
	return monitorInstance
}

// Start begins the offer monitoring service
func (m *Monitor) Start() error {
	tickRate := time.Duration(m.config.OfferManagerOptions.OfferMonitorTickRate) * time.Millisecond

	log.Printf("Starting Offer Monitor for account: %s", m.config.PublicKey)
	log.Printf("Tick Rate: %dms | Horizon: %s", m.config.OfferManagerOptions.OfferMonitorTickRate, m.horizonBaseURI)

	ticker := time.NewTicker(tickRate)
	defer ticker.Stop()

	// Fetch offers immediately on start (do not exit on error)
	if err := m.fetchAndReconcileOffers(); err != nil {
		log.Printf("Initial offer fetch error: %v", err)
	}

	for {
		select {
		case t := <-ticker.C:
			log.Printf("[OFFER TICK] %s", t.Format(time.RFC3339))
			if err := m.fetchAndReconcileOffers(); err != nil {
				log.Printf("Offer fetch error: %v", err)
			}
		case <-m.stopChan:
			log.Println("Offer monitor stopped")
			return nil
		}
	}
}

// fetchAndReconcileOffers retrieves current offers from Horizon and reconciles with local hashmap
func (m *Monitor) fetchAndReconcileOffers() error {
	url := fmt.Sprintf("%s/accounts/%s/offers", m.horizonBaseURI, m.config.PublicKey)

	resp, err := m.httpClient.Get(url)
	if err != nil {
		return fmt.Errorf("failed to fetch offers: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("horizon API returned status %d: %s", resp.StatusCode, string(body))
	}

	var offersResponse HorizonOffersResponse
	if err := json.NewDecoder(resp.Body).Decode(&offersResponse); err != nil {
		return fmt.Errorf("failed to decode offers response: %w", err)
	}

	// Reconcile offers
	m.reconcileOffers(offersResponse.Embedded.Records)

	return nil
}

// reconcileOffers updates the local hashmap based on current market state
func (m *Monitor) reconcileOffers(horizonOffers []HorizonOffer) {
	m.mu.Lock()
	defer m.mu.Unlock()

	// Build a set of current offer IDs from Horizon
	currentOfferIDs := make(map[string]bool)
	for _, hOffer := range horizonOffers {
		// Only process offers for our trading pair
		if !m.isRelevantOffer(&hOffer) {
			continue
		}

		currentOfferIDs[hOffer.ID] = true

		// Determine offer type
		offerType := m.determineOfferType(&hOffer)

		// Normalize price and quantity (bids are inverted in Horizon)
		normalizedPrice := hOffer.Price
		normalizedQuantity := hOffer.Amount
		
		if offerType == OfferTypeBid {
			// For ManageBuyOffer:
			// - Horizon price is EURC/XLM (selling/buying)
			// - Horizon amount is in EURC (what you're selling)
			// We want: price in XLM/EURC and amount in XLM
			horizonPrice, _ := strconv.ParseFloat(hOffer.Price, 64)
			horizonAmount, _ := strconv.ParseFloat(hOffer.Amount, 64)
			
			if horizonPrice > 0 {
				// Invert price: XLM/EURC = 1 / (EURC/XLM)
				normalizedPrice = fmt.Sprintf("%.7f", 1.0/horizonPrice)
				// Convert amount to XLM: XLM = EURC / (EURC/XLM)
				normalizedQuantity = fmt.Sprintf("%.7f", horizonAmount/horizonPrice)
			}
		}

		// Check if this offer already exists in our hashmap
		if existingOffer, exists := m.offers[hOffer.ID]; exists {
			// Offer exists - check if quantity has changed (partial fill)
			if existingOffer.Quantity != normalizedQuantity {
				log.Printf("[OFFER UPDATE] ID=%s Type=%s Quantity changed: %s -> %s",
					hOffer.ID, offerType, existingOffer.Quantity, normalizedQuantity)
				existingOffer.Quantity = normalizedQuantity
				existingOffer.Price = normalizedPrice
			}
		} else {
			// New offer not in our hashmap - add it
			log.Printf("[OFFER NEW] ID=%s Type=%s Quantity=%s Price=%s",
				hOffer.ID, offerType, normalizedQuantity, normalizedPrice)
			m.offers[hOffer.ID] = &Offer{
				OfferID:  hOffer.ID,
				Quantity: normalizedQuantity,
				Price:    normalizedPrice,
				Type:     offerType,
			}
		}
	}

	// Remove offers from hashmap that are no longer in market (filled or cancelled)
	for offerID, offer := range m.offers {
		if !currentOfferIDs[offerID] {
			log.Printf("[OFFER REMOVED] ID=%s Type=%s (filled or cancelled)", offerID, offer.Type)
			delete(m.offers, offerID)
		}
	}

	// Log current state if there are offers
	if len(m.offers) > 0 {
		m.logOfferState()
	}
}

// isRelevantOffer checks if the offer is for our configured trading pair
func (m *Monitor) isRelevantOffer(offer *HorizonOffer) bool {
	// Check if this offer is for our trading pair
	// For a bid: we're buying base asset (selling counter asset)
	// For an ask: we're selling base asset (buying counter asset)

	isBid := m.isAsset(&offer.Buying, m.config.BaseAsset, m.config.BaseAssetIssuer) &&
		m.isAsset(&offer.Selling, m.config.CounterAsset, m.config.CounterAssetIssuer)

	isAsk := m.isAsset(&offer.Selling, m.config.BaseAsset, m.config.BaseAssetIssuer) &&
		m.isAsset(&offer.Buying, m.config.CounterAsset, m.config.CounterAssetIssuer)

	return isBid || isAsk
}

// determineOfferType determines if an offer is a bid or ask
func (m *Monitor) determineOfferType(offer *HorizonOffer) OfferType {
	// If we're buying base asset, it's a bid
	if m.isAsset(&offer.Buying, m.config.BaseAsset, m.config.BaseAssetIssuer) {
		return OfferTypeBid
	}
	return OfferTypeAsk
}

// isAsset checks if an asset matches the given asset code and issuer
func (m *Monitor) isAsset(asset *Asset, assetCode, assetIssuer string) bool {
	if assetIssuer == "native" {
		return asset.AssetType == "native"
	}
	return asset.AssetCode == assetCode && asset.AssetIssuer == assetIssuer
}

// logOfferState logs the current state of all offers
func (m *Monitor) logOfferState() {
	log.Printf("\n📊 [OFFERS] Current State (%d offers)", len(m.offers))
	for _, offer := range m.offers {
		qty, _ := strconv.ParseFloat(offer.Quantity, 64)
		price, _ := strconv.ParseFloat(offer.Price, 64)
		log.Printf("   %s: ID=%s | Qty=%.4f | Price=%.6f",
			offer.Type, offer.OfferID, qty, price)
	}
}

// GetOffers returns a copy of the current offers map
func (m *Monitor) GetOffers() map[string]*Offer {
	m.mu.RLock()
	defer m.mu.RUnlock()

	// Return a copy to avoid race conditions
	offersCopy := make(map[string]*Offer)
	for k, v := range m.offers {
		offersCopy[k] = &Offer{
			OfferID:  v.OfferID,
			Quantity: v.Quantity,
			Price:    v.Price,
			Type:     v.Type,
		}
	}
	return offersCopy
}

// Stop gracefully stops the offer monitor
func (m *Monitor) Stop() {
	log.Println("Stopping Offer Monitor...")
	close(m.stopChan)
}
