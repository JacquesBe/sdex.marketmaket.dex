package offers

import (
	"encoding/json"
	"fmt"
	"log"
	"math"
	"strconv"

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

// Manager handles offer execution on the Stellar DEX
type Manager struct {
	config         *config.BotConfig
	horizonClient  *horizonclient.Client
	sourcekeypair  *keypair.Full
	networkPass    string
	monitor        *Monitor
}

// NewManager creates a new offer manager
func NewManager(botConfig *config.BotConfig, horizonBaseURI string, networkPassphrase string, monitor *Monitor) (*Manager, error) {
	// Parse the secret key
	kp, err := keypair.ParseFull(botConfig.SecretKey)
	if err != nil {
		return nil, fmt.Errorf("failed to parse secret key: %w", err)
	}

	// Create Horizon client
	client := &horizonclient.Client{
		HorizonURL: horizonBaseURI,
	}

	return &Manager{
		config:         botConfig,
		horizonClient:  client,
		sourcekeypair:  kp,
		networkPass:    networkPassphrase,
		monitor:        monitor,
	}, nil
}

// ExecuteOffers evaluates current offers and submits/updates as needed
// bidPrice and askPrice are the new prices calculated by the strategy
func (m *Manager) ExecuteOffers(bidPrice, askPrice float64) error {
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

	// Evaluate and execute bid side
	if err := m.evaluateAndExecute(currentBid, bidPrice, OfferTypeBid); err != nil {
		log.Printf("[OFFER MANAGER] Error executing bid: %v", err)
	}

	// Evaluate and execute ask side (independent of bid)
	if err := m.evaluateAndExecute(currentAsk, askPrice, OfferTypeAsk); err != nil {
		log.Printf("[OFFER MANAGER] Error executing ask: %v", err)
	}

	return nil
}

// evaluateAndExecute checks if an offer needs updating and executes if necessary
func (m *Manager) evaluateAndExecute(currentOffer *Offer, newPrice float64, offerType OfferType) error {
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
		priceDiffBps := (priceDiff / currentPrice) * 10000

		if priceDiffBps > m.config.OfferManagerOptions.PriceGraceBasisPoints {
			needsUpdate = true
			reason = fmt.Sprintf("price diff %.2f bps > grace %.2f bps", priceDiffBps, m.config.OfferManagerOptions.PriceGraceBasisPoints)
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

	if !needsUpdate {
		log.Printf("[OFFER MANAGER] %s offer OK - no update needed", offerType)
		return nil
	}

	log.Printf("[OFFER MANAGER] %s offer needs update: %s", offerType, reason)

	// Execute the offer
	return m.submitOfferWithRetry(offerID, newPrice, offerType, 0)
}

// submitOfferWithRetry submits or updates an offer on the Stellar DEX with retry logic
func (m *Manager) submitOfferWithRetry(offerID int64, price float64, offerType OfferType, retryCount int) error {
	// Prevent infinite recursion - allow up to 3 retries
	if retryCount > 3 {
		return fmt.Errorf("max retries (3) exceeded for %s offer", offerType)
	}
	return m.submitOffer(offerID, price, offerType, retryCount)
}

// submitOffer submits or updates an offer on the Stellar DEX
func (m *Manager) submitOffer(offerID int64, price float64, offerType OfferType, retryCount int) error {
	// Get source account
	accountRequest := horizonclient.AccountRequest{AccountID: m.config.PublicKey}
	sourceAccount, err := m.horizonClient.AccountDetail(accountRequest)
	if err != nil {
		return fmt.Errorf("failed to load source account: %w", err)
	}

	// Build assets
	baseAsset := m.buildAsset(m.config.BaseAsset, m.config.BaseAssetIssuer)
	counterAsset := m.buildAsset(m.config.CounterAsset, m.config.CounterAssetIssuer)

	// Build the operation based on offer type
	var operation txnbuild.Operation
	amount := fmt.Sprintf("%.7f", m.config.OfferManagerOptions.OfferQuantity)
	priceStr := fmt.Sprintf("%.7f", price)

	// Convert price string to xdr.Price
	xdrPrice, err := stprice.Parse(priceStr)
	if err != nil {
		return fmt.Errorf("failed to parse price: %w", err)
	}

	if offerType == OfferTypeBid {
		// Bid = Manage Buy Offer (buying base asset with counter asset)
		operation = &txnbuild.ManageBuyOffer{
			Selling:       counterAsset,
			Buying:        baseAsset,
			Amount:        amount,
			Price:         xdrPrice,
			OfferID:       offerID,
			SourceAccount: m.config.PublicKey,
		}
	} else {
		// Ask = Manage Sell Offer (selling base asset for counter asset)
		operation = &txnbuild.ManageSellOffer{
			Selling:       baseAsset,
			Buying:        counterAsset,
			Amount:        amount,
			Price:         xdrPrice,
			OfferID:       offerID,
			SourceAccount: m.config.PublicKey,
		}
	}

	// Build transaction
	tx, err := txnbuild.NewTransaction(
		txnbuild.TransactionParams{
			SourceAccount:        &sourceAccount,
			IncrementSequenceNum: true,
			Operations:           []txnbuild.Operation{operation},
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

	// Submit transaction
	log.Printf("[OFFER MANAGER] Submitting %s offer: offerID=%d, price=%.6f, amount=%.4f",
		offerType, offerID, price, m.config.OfferManagerOptions.OfferQuantity)

	resp, err := m.horizonClient.SubmitTransaction(tx)
	if err != nil {
		// Check if it's an op_offer_not_found error
		if hError, ok := err.(*horizonclient.Error); ok {
			if m.isOfferNotFoundError(hError) {
				log.Printf("[OFFER MANAGER] Offer not found, retrying with new offer (offerID=0)")
				// Retry with offerID = 0 to create a new offer (increment retry counter)
				return m.submitOffer(0, price, offerType, retryCount+1)
			}
		}
		return fmt.Errorf("failed to submit transaction: %w", err)
	}

	log.Printf("[OFFER MANAGER] ✅ %s offer submitted successfully (tx hash: %s)", offerType, resp.Hash)
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

// isOfferNotFoundError checks if the error is an op_offer_not_found error
func (m *Manager) isOfferNotFoundError(hError *horizonclient.Error) bool {
	if hError.Problem.Extras != nil {
		if resultCodes, ok := hError.Problem.Extras["result_codes"].(map[string]interface{}); ok {
			if operations, ok := resultCodes["operations"].([]interface{}); ok {
				for _, op := range operations {
					if opCode, ok := op.(string); ok && opCode == "op_offer_not_found" {
						return true
					}
				}
			}
		}
	}
	return false
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
	resp, err := m.horizonClient.HTTP.Get(url)
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
