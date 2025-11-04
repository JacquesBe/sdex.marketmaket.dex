package offers

import (
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/jacquesbecker/sdex-marketmaker/config"
	"github.com/stellar/go/clients/horizonclient"
	"github.com/stellar/go/keypair"
	stprice "github.com/stellar/go/price"
	"github.com/stellar/go/txnbuild"
)

// OfferType represents whether an offer is a bid or ask
type OfferType string

const (
	OfferTypeBid OfferType = "bid"
	OfferTypeAsk OfferType = "ask"
)

// Offer represents a single offer in the market
type Offer struct {
	OfferID  string    // Stellar offer ID
	Quantity string    // Amount of base asset
	Price    string    // Price (counter asset per base asset)
	Type     OfferType // bid or ask
}

// StrategyPriceProvider is an interface for getting latest prices from strategy
type StrategyPriceProvider interface {
	GetLatestPrices() (bid float64, ask float64, ok bool)
}

// Manager handles offer execution on the Stellar DEX
type Manager struct {
	config         *config.BotConfig
	horizonClient  *horizonclient.Client
	httpClient     *http.Client
	sourcekeypair  *keypair.Full
	networkPass    string
	monitor        *Monitor
	strategy       StrategyPriceProvider
	submitMutex    sync.Mutex // Prevents concurrent transaction submissions
}

// NewManager creates a new offer manager
func NewManager(botConfig *config.BotConfig, horizonBaseURI string, networkPassphrase string, monitor *Monitor, strategy StrategyPriceProvider) (*Manager, error) {
	// Parse the secret key
	kp, err := keypair.ParseFull(botConfig.SecretKey)
	if err != nil {
		return nil, fmt.Errorf("failed to parse secret key: %w", err)
	}

	// Create Horizon client
	client := &horizonclient.Client{
		HorizonURL: horizonBaseURI,
	}

	// Create HTTP client
	httpClient := &http.Client{
		Timeout: 10 * time.Second,
	}

	return &Manager{
		config:         botConfig,
		horizonClient:  client,
		httpClient:     httpClient,
		sourcekeypair:  kp,
		networkPass:    networkPassphrase,
		monitor:        monitor,
		strategy:       strategy,
	}, nil
}

// OfferToSubmit represents an offer operation to submit
type OfferToSubmit struct {
	OfferID int64
	Price   float64
	Type    OfferType
}

// ExecuteOffers evaluates current offers and submits/updates as needed
// Now gets prices from Strategy instead of parameters
func (m *Manager) ExecuteOffers() error {
	// Check if already submitting - non-blocking check
	if !m.submitMutex.TryLock() {
		log.Printf("[OFFER MANAGER] Submission already in progress, skipping this tick")
		return nil
	}
	defer m.submitMutex.Unlock()

	// Get latest prices from strategy
	bidPrice, askPrice, ok := m.strategy.GetLatestPrices()
	if !ok {
		log.Printf("[OFFER MANAGER] No prices available from strategy yet, skipping")
		return nil
	}
	// Sanity check prices
	if bidPrice <= 0 || askPrice <= 0 {
		return fmt.Errorf("invalid prices: bid=%.6f, ask=%.6f (must be positive)", bidPrice, askPrice)
	}
	if bidPrice >= askPrice {
		return fmt.Errorf("crossed prices: bid=%.6f >= ask=%.6f (bid must be < ask)", bidPrice, askPrice)
	}

	// Get current offers from monitor
	currentOffers := m.monitor.GetOffers()

	// Log current state
	m.logCurrentOffers(currentOffers)

	// Find current bid and ask offers
	var currentBid, currentAsk *Offer
	for _, offer := range currentOffers {
		if offer.Type == OfferTypeBid {
			currentBid = offer
		} else if offer.Type == OfferTypeAsk {
			currentAsk = offer
		}
	}

	// Build list of offers to submit
	var offersToSubmit []OfferToSubmit

	// Evaluate bid
	bidNeedsUpdate, bidOfferID, bidReason := m.evaluateOffer(currentBid, bidPrice, OfferTypeBid)
	if bidNeedsUpdate {
		log.Printf("[OFFER MANAGER] BID needs update: %s", bidReason)
		offersToSubmit = append(offersToSubmit, OfferToSubmit{
			OfferID: bidOfferID,
			Price:   bidPrice,
			Type:    OfferTypeBid,
		})
	}

	// Evaluate ask
	askNeedsUpdate, askOfferID, askReason := m.evaluateOffer(currentAsk, askPrice, OfferTypeAsk)
	if askNeedsUpdate {
		log.Printf("[OFFER MANAGER] ASK needs update: %s", askReason)
		offersToSubmit = append(offersToSubmit, OfferToSubmit{
			OfferID: askOfferID,
			Price:   askPrice,
			Type:    OfferTypeAsk,
		})
	}

	// If nothing to submit, we're done
	if len(offersToSubmit) == 0 {
		log.Println("[OFFER MANAGER] All offers OK - no update needed")
		return nil
	}

	// Submit all needed offers in one transaction
	return m.submitOffers(offersToSubmit, 0)
}

// evaluateOffer checks if an offer needs updating and returns the evaluation results
// Returns: (needsUpdate bool, offerID int64, reason string)
func (m *Manager) evaluateOffer(currentOffer *Offer, newPrice float64, offerType OfferType) (bool, int64, string) {
	needsUpdate := false
	var offerID int64 = 0 // 0 means create new offer
	reason := ""

	if currentOffer == nil {
		// No offer exists - need to create one
		needsUpdate = true
		reason = "no offer exists"
	} else {
		// Offer exists - check if it needs updating
		offerID, _ = strconv.ParseInt(currentOffer.OfferID, 10, 64)

		// Check price grace
		currentPrice, _ := strconv.ParseFloat(currentOffer.Price, 64)
		priceDiff := math.Abs(currentPrice - newPrice)
		priceDiffBps := math.Floor((priceDiff / currentPrice) * 10000)

		if priceDiffBps > m.config.OfferManagerOptions.PriceGraceBasisPoints {
			needsUpdate = true
			reason = fmt.Sprintf("price diff %.0f bps > grace %.2f bps", priceDiffBps, m.config.OfferManagerOptions.PriceGraceBasisPoints)
		}

		// Check quantity grace
		currentQty, _ := strconv.ParseFloat(currentOffer.Quantity, 64)
		qtyDiff := math.Abs(currentQty - m.config.OfferManagerOptions.OfferQuantity)

		if qtyDiff > m.config.OfferManagerOptions.OfferGraceQuantity {
			needsUpdate = true
			if reason != "" {
				reason += " AND "
			}
			reason += fmt.Sprintf("qty diff %.4f > grace %.4f", qtyDiff, m.config.OfferManagerOptions.OfferGraceQuantity)
		}
	}

	return needsUpdate, offerID, reason
}

// submitOffers submits all offers in a SINGLE transaction
func (m *Manager) submitOffers(offers []OfferToSubmit, retryCount int) error {
	// Prevent infinite recursion
	if retryCount > 3 {
		return fmt.Errorf("max retries (3) exceeded for offer submission")
	}

	// Get source account
	accountRequest := horizonclient.AccountRequest{AccountID: m.config.PublicKey}
	sourceAccount, err := m.horizonClient.AccountDetail(accountRequest)
	if err != nil {
		return fmt.Errorf("failed to load source account: %w", err)
	}

	// Build assets
	baseAsset := m.buildAsset(m.config.BaseAsset, m.config.BaseAssetIssuer)
	counterAsset := m.buildAsset(m.config.CounterAsset, m.config.CounterAssetIssuer)
	amount := fmt.Sprintf("%.7f", m.config.OfferManagerOptions.OfferQuantity)

	// Build operations
	var operations []txnbuild.Operation
	for _, offer := range offers {
		priceStr := fmt.Sprintf("%.7f", offer.Price)
		xdrPrice, err := stprice.Parse(priceStr)
		if err != nil {
			return fmt.Errorf("failed to parse price for %s: %w", offer.Type, err)
		}

		if offer.Type == OfferTypeBid {
			operations = append(operations, &txnbuild.ManageBuyOffer{
				Selling:       counterAsset,
				Buying:        baseAsset,
				Amount:        amount,
				Price:         xdrPrice,
				OfferID:       offer.OfferID,
				SourceAccount: m.config.PublicKey,
			})
		} else {
			operations = append(operations, &txnbuild.ManageSellOffer{
				Selling:       baseAsset,
				Buying:        counterAsset,
				Amount:        amount,
				Price:         xdrPrice,
				OfferID:       offer.OfferID,
				SourceAccount: m.config.PublicKey,
			})
		}
	}

	// Build transaction
	tx, err := txnbuild.NewTransaction(
		txnbuild.TransactionParams{
			SourceAccount:        &sourceAccount,
			IncrementSequenceNum: true,
			Operations:           operations,
			BaseFee:              m.config.OfferManagerOptions.FeeMaxStroops,
			Preconditions: txnbuild.Preconditions{
				TimeBounds: txnbuild.NewTimeout(300),
			},
		},
	)
	if err != nil {
		return fmt.Errorf("failed to build transaction: %w", err)
	}

	// Sign transaction
	tx, err = tx.Sign(m.networkPass, m.sourcekeypair)
	if err != nil {
		return fmt.Errorf("failed to sign transaction: %w", err)
	}

	// Log submission
	if len(offers) == 2 {
		log.Printf("🚀 [OFFER MANAGER] Submitting BOTH offers in single transaction")
	} else if offers[0].Type == OfferTypeBid {
		log.Printf("🟢 [OFFER MANAGER] Submitting BID offer only")
	} else {
		log.Printf("🔴 [OFFER MANAGER] Submitting ASK offer only")
	}

	// Submit transaction
	resp, err := m.horizonClient.SubmitTransaction(tx)
	if err != nil {
		// Check for op_offer_not_found errors and which operations failed
		if hError, ok := err.(*horizonclient.Error); ok {
			failedIndices := m.getFailedOfferIndices(hError)
			if len(failedIndices) > 0 {
				log.Printf("[OFFER MANAGER] Offers not found at indices %v, retrying with offerID=0", failedIndices)
				
				// Set failed offers to offerID=0 (create new)
				retryOffers := make([]OfferToSubmit, len(offers))
				copy(retryOffers, offers)
				for _, idx := range failedIndices {
					if idx < len(retryOffers) {
						retryOffers[idx].OfferID = 0
					}
				}
				
				return m.submitOffers(retryOffers, retryCount+1)
			}
		}
		return fmt.Errorf("failed to submit transaction: %w", err)
	}

	log.Printf("[OFFER MANAGER] ✅ Offers submitted successfully (tx hash: %s)", resp.Hash)
	
	// Force monitor to refresh immediately to sync hashmap with new on-chain state
	if err := m.monitor.ForceRefresh(); err != nil {
		log.Printf("[OFFER MANAGER] Warning: Failed to refresh monitor after submission: %v", err)
	}
	
	return nil
}

// buildAsset creates a Stellar asset from config
func (m *Manager) buildAsset(assetCode, assetIssuer string) txnbuild.Asset {
	if assetIssuer == "native" {
		return txnbuild.NativeAsset{}
	}
	return txnbuild.CreditAsset{
		Code:   assetCode,
		Issuer: assetIssuer,
	}
}

// getFailedOfferIndices returns the indices of operations that failed with op_offer_not_found
func (m *Manager) getFailedOfferIndices(hError *horizonclient.Error) []int {
	var failedIndices []int
	
	if hError.Problem.Extras != nil {
		if resultCodes, ok := hError.Problem.Extras["result_codes"].(map[string]interface{}); ok {
			if operations, ok := resultCodes["operations"].([]interface{}); ok {
				for i, op := range operations {
					if opCode, ok := op.(string); ok && opCode == "op_offer_not_found" {
						failedIndices = append(failedIndices, i)
					}
				}
			}
		}
	}
	
	return failedIndices
}

// logCurrentOffers logs the current state of offers from the monitor
func (m *Manager) logCurrentOffers(offers map[string]*Offer) {
	if len(offers) == 0 {
		log.Printf("[OFFER MANAGER] Current offers: NONE")
		return
	}

	log.Printf("[OFFER MANAGER] Current offers from monitor:")
	for _, offer := range offers {
		qty, _ := strconv.ParseFloat(offer.Quantity, 64)
		price, _ := strconv.ParseFloat(offer.Price, 64)
		log.Printf("  %s: ID=%s | Qty=%.4f | Price=%.6f",
			offer.Type, offer.OfferID, qty, price)
	}
}

// CancelAllOffers cancels all active offers for this account
func (m *Manager) CancelAllOffers() error {
	log.Println("[OFFER MANAGER] Cancelling all offers...")

	// Fetch all current offers from Horizon directly
	url := fmt.Sprintf("%s/accounts/%s/offers", m.horizonClient.HorizonURL, m.config.PublicKey)
	resp, err := m.httpClient.Get(url)
	if err != nil {
		return fmt.Errorf("failed to fetch offers: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != 200 {
		return fmt.Errorf("horizon API returned status %d", resp.StatusCode)
	}

	var offersResponse struct {
		Embedded struct {
			Records []struct {
				ID      string `json:"id"`
				Selling Asset  `json:"selling"`
				Buying  Asset  `json:"buying"`
			} `json:"records"`
		} `json:"_embedded"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&offersResponse); err != nil {
		return fmt.Errorf("failed to decode offers: %w", err)
	}

	offers := offersResponse.Embedded.Records
	if len(offers) == 0 {
		log.Println("[OFFER MANAGER] No offers to cancel")
		return nil
	}

	log.Printf("[OFFER MANAGER] Found %d offers to cancel", len(offers))

	// Get source account
	accountRequest := horizonclient.AccountRequest{AccountID: m.config.PublicKey}
	sourceAccount, err := m.horizonClient.AccountDetail(accountRequest)
	if err != nil {
		return fmt.Errorf("failed to load source account: %w", err)
	}

	// Build operations to cancel each offer
	var operations []txnbuild.Operation
	for _, offer := range offers {
		offerID, _ := strconv.ParseInt(offer.ID, 10, 64)
		log.Printf("[OFFER MANAGER] Cancelling offer ID: %s", offer.ID)

		// Create delete operation (amount = "0")
		sellingAsset := m.buildAssetFromHorizon(&offer.Selling)
		buyingAsset := m.buildAssetFromHorizon(&offer.Buying)

		operations = append(operations, &txnbuild.ManageSellOffer{
			Selling:       sellingAsset,
			Buying:        buyingAsset,
			Amount:        "0", // Zero amount cancels the offer
			Price:         stprice.MustParse("1"),
			OfferID:       offerID,
			SourceAccount: m.config.PublicKey,
		})
	}

	// Build transaction with all cancel operations
	tx, err := txnbuild.NewTransaction(
		txnbuild.TransactionParams{
			SourceAccount:        &sourceAccount,
			IncrementSequenceNum: true,
			Operations:           operations,
			BaseFee:              m.config.OfferManagerOptions.FeeMaxStroops,
			Preconditions: txnbuild.Preconditions{
				TimeBounds: txnbuild.NewTimeout(300),
			},
		},
	)
	if err != nil {
		return fmt.Errorf("failed to build cancel transaction: %w", err)
	}

	// Sign transaction
	tx, err = tx.Sign(m.networkPass, m.sourcekeypair)
	if err != nil {
		return fmt.Errorf("failed to sign cancel transaction: %w", err)
	}

	// Submit transaction
	resp2, err := m.horizonClient.SubmitTransaction(tx)
	if err != nil {
		return fmt.Errorf("failed to submit cancel transaction: %w", err)
	}

	log.Printf("[OFFER MANAGER] ✅ All offers cancelled successfully (tx hash: %s)", resp2.Hash)
	
	// Force monitor to refresh immediately to clear hashmap
	if err := m.monitor.ForceRefresh(); err != nil {
		log.Printf("[OFFER MANAGER] Warning: Failed to refresh monitor after cancellation: %v", err)
	}
	
	return nil
}

// buildAssetFromHorizon creates a Stellar asset from Horizon API response
func (m *Manager) buildAssetFromHorizon(asset *Asset) txnbuild.Asset {
	if asset.AssetType == "native" {
		return txnbuild.NativeAsset{}
	}
	return txnbuild.CreditAsset{
		Code:   asset.AssetCode,
		Issuer: asset.AssetIssuer,
	}
}
