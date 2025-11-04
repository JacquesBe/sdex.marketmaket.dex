package strategy

import (
	"fmt"
	"log"
	"sync"

	"github.com/jacquesbecker/sdex-marketmaker/balance"
	"github.com/jacquesbecker/sdex-marketmaker/config"
	"github.com/jacquesbecker/sdex-marketmaker/pricefeed"
)

// StrategyEngine manages the market making strategy
type StrategyEngine struct {
	Feed           pricefeed.PriceFeed
	BotConfig      *config.BotConfig
	mu             sync.RWMutex
	latestBid      float64
	latestAsk      float64
	hasPrices      bool
}

// NewStrategyEngine creates a new strategy engine with given feed and config
func NewStrategyEngine(feed pricefeed.PriceFeed, botConfig *config.BotConfig) *StrategyEngine {
	return &StrategyEngine{
		Feed:      feed,
		BotConfig: botConfig,
	}
}

// getBalanceMonitor returns the global balance monitor instance
func (s *StrategyEngine) getBalanceMonitor() *balance.Monitor {
	return balance.GetInstance()
}

// ComputeQuotes calculates bid and ask prices using custom model
// Returns (pBid, pAsk, ok) where ok indicates if quotes are valid
func (s *StrategyEngine) ComputeQuotes() (pBid, pAsk float64, ok bool) {
	// Fetch latest feature snapshot
	features, hasFeatures := s.Feed.GetLatestFeatures()
	if !hasFeatures {
		log.Printf("[STRATEGY] No features available yet")
		return 0, 0, false
	}

	// Check if rolling volatility is available
	if features.RollingVolatility == nil {
		log.Printf("[STRATEGY] Rolling volatility not yet available")
		return 0, 0, false
	}
	rollingVol := *features.RollingVolatility

	// Get balances from balance monitor
	monitor := s.getBalanceMonitor()
	var qBase, qQuote float64
	if monitor != nil {
		baseBalance, counterBalance, err := monitor.GetLatestBalances()
		if err != nil {
			log.Printf("[STRATEGY] No balances available yet, skipping quote computation")
			return 0, 0, false
		}
		if _, err := fmt.Sscanf(baseBalance.Balance, "%f", &qBase); err != nil {
			qBase = 0
		}
		if _, err := fmt.Sscanf(counterBalance.Balance, "%f", &qQuote); err != nil {
			qQuote = 0
		}
	} else {
		log.Printf("[STRATEGY] Balance monitor not initialized, skipping quote computation")
		return 0, 0, false
	}

	// Get strategy parameters from bot config
	blendWeight := s.BotConfig.StrategyOptions.BlendWeight
	halfSpreadFloorBps := s.BotConfig.StrategyOptions.HalfSpreadFloor // in basis points
	volatilitySensitivity := s.BotConfig.StrategyOptions.VolatilitySensitivity
	inventoryBias := s.BotConfig.StrategyOptions.InventoryBias
	orderBookBias := s.BotConfig.StrategyOptions.OBImbalanceSensitivity

	// 1. Calculate fair price: (1 - blendWeight) * externalMidPrice + blendWeight * microprice
	fairPrice := (1-blendWeight)*features.ExternalMidPrice + blendWeight*features.MicroPrice
	if fairPrice <= 0 {
		log.Printf("[STRATEGY] Invalid fair price: %.6f", fairPrice)
		return 0, 0, false
	}
	
	// Calculate total wallet value in base asset terms using fair price
	quoteInBase := qQuote / fairPrice
	totalWalletBase := qBase + quoteInBase
	log.Printf("[INVENTORY] Total Wallet Value: %.4f %s (%.4f %s + %.4f %s in base terms at fair price)", 
		totalWalletBase, s.BotConfig.BaseAsset, qBase, s.BotConfig.BaseAsset, quoteInBase, s.BotConfig.BaseAsset)

	// 2. Calculate relative inventory: (qBase * ExtMid) / ((qBase * ExtMid) + qQuote) - 0.5
	// Range: [-0.5, 0.5] where 0 = balanced
	baseValue := qBase * features.ExternalMidPrice
	totalValue := baseValue + qQuote
	var relativeInventory float64
	if totalValue > 0 {
		relativeInventory = (baseValue / totalValue) - 0.5
	}
	
	// Log inventory calculation details
	log.Printf("[INVENTORY] qBase=%.4f, qQuote=%.4f, ExtMid=%.6f", qBase, qQuote, features.ExternalMidPrice)
	log.Printf("[INVENTORY] baseValue=%.4f, totalValue=%.4f", baseValue, totalValue)
	log.Printf("[INVENTORY] relativeInventory=%.6f (%.2f%%)", relativeInventory, relativeInventory*100)

	// 3. Convert halfSpreadFloor from bps to price
	halfSpreadFloor := (halfSpreadFloorBps / 10000.0) * features.ExternalMidPrice
	
	// 4. Calculate volatility component (already in price units)
	volComponent := volatilitySensitivity * rollingVol
	volComponentBps := (volComponent / features.ExternalMidPrice) * 10000
	
	// 5. Calculate asymmetric inventory adjustment
	// When long XLM (relativeInventory > 0): widen bid to discourage buying more
	// When short XLM (relativeInventory < 0): widen ask to discourage selling more
	invComponentBid := inventoryBias * max(0, relativeInventory) * fairPrice
	invComponentAsk := inventoryBias * max(0, -relativeInventory) * fairPrice
	
	// Log inventory adjustment calculation
	log.Printf("[INVENTORY ADJ] relativeInventory=%.6f | inventoryBias=%.6f", 
		relativeInventory, inventoryBias)
	log.Printf("[INVENTORY ADJ] invComponentBid=%.8f | invComponentAsk=%.8f", invComponentBid, invComponentAsk)
	
	// 6. Calculate orderbook imbalance penalties
	// BidPenalty high = orderbook ask side heavy (sellers) -> widen YOUR bid (you compete with sellers)
	// AskPenalty high = orderbook bid side heavy (buyers) -> widen YOUR ask (you compete with buyers)
	obPenaltyBid := orderBookBias * features.BidPenalty * fairPrice
	obPenaltyAsk := orderBookBias * features.AskPenalty * fairPrice
	
	log.Printf("[OB IMBALANCE] BidPenalty=%.6f | AskPenalty=%.6f | orderBookBias=%.6f", 
		features.BidPenalty, features.AskPenalty, orderBookBias)
	log.Printf("[OB IMBALANCE] obPenaltyBid=%.8f | obPenaltyAsk=%.8f", obPenaltyBid, obPenaltyAsk)
	
	// 7. Calculate half spreads asymmetrically
	halfSpreadBid := halfSpreadFloor + volComponent + invComponentBid + obPenaltyBid
	halfSpreadAsk := halfSpreadFloor + volComponent + invComponentAsk + obPenaltyAsk
	
	// 7b. Enforce floor - ensure half spreads NEVER go below minimum
	halfSpreadBid = max(halfSpreadBid, halfSpreadFloor)
	halfSpreadAsk = max(halfSpreadAsk, halfSpreadFloor)

	// 8. Calculate final bid and ask prices
	pBid = fairPrice - halfSpreadBid
	pAsk = fairPrice + halfSpreadAsk

	// Calculate spreads and metrics for logging (before sanity checks so we always see the breakdown)
	spread := pAsk - pBid
	spreadBps := (spread / fairPrice) * 10000
	halfSpreadBidBps := (halfSpreadBid / fairPrice) * 10000
	halfSpreadAskBps := (halfSpreadAsk / fairPrice) * 10000
	
	// Calculate bid and ask spread from fair price in basis points
	bidSpreadFromFair := fairPrice - pBid
	bidSpreadFromFairBps := (bidSpreadFromFair / fairPrice) * 10000
	askSpreadFromFair := pAsk - fairPrice
	askSpreadFromFairBps := (askSpreadFromFair / fairPrice) * 10000

	// Row 1: Input metrics
	log.Printf("\n💰 [STRATEGY INVOCATION]")
	log.Printf("   ExtMidPrice: %.6f | MicroPrice: %.6f | AbsoluteVol: %.8f", 
		features.ExternalMidPrice, features.MicroPrice, rollingVol)
	log.Printf("   RelativeInventory: %.6f (%.1f%%)", relativeInventory, relativeInventory*100)
	
	// Row 2: Calculated intermediate values
	log.Printf("   FairPrice: %.6f | InventoryBias: %.6f", fairPrice, inventoryBias)
	
	// Detailed breakdown of half spread components (asymmetric)
	log.Printf("   📊 [HALF SPREAD BREAKDOWN]")
	log.Printf("      Floor: %.6f (%.2f bps) | Vol: %.6f (%.2f bps)",
		halfSpreadFloor, halfSpreadFloorBps, volComponent, volComponentBps)
	log.Printf("      BID HalfSpread: %.6f (%.2f bps) | ASK HalfSpread: %.6f (%.2f bps)",
		halfSpreadBid, halfSpreadBidBps, halfSpreadAsk, halfSpreadAskBps)
	
	// Row 3: Final quotes with spreads from fair price
	log.Printf("   🟢 BID: %.6f (%.2f bps from fair) | 🔴 ASK: %.6f (%.2f bps from fair)", 
		pBid, bidSpreadFromFairBps, pAsk, askSpreadFromFairBps)
	log.Printf("   Total Spread: %.6f (%.2f bps)\n", spread, spreadBps)

	// Sanity checks
	if pBid <= 0 || pAsk <= 0 {
		log.Printf("❌ [STRATEGY ERROR] Invalid prices: pBid=%.6f pAsk=%.6f\n", pBid, pAsk)
		return 0, 0, false
	}

	if pBid >= pAsk {
		log.Printf("❌ [STRATEGY ERROR] Crossed quotes: pBid=%.6f >= pAsk=%.6f\n", pBid, pAsk)
		return 0, 0, false
	}

	// Store latest prices
	s.mu.Lock()
	s.latestBid = pBid
	s.latestAsk = pAsk
	s.hasPrices = true
	s.mu.Unlock()

	return pBid, pAsk, true
}

// GetLatestPrices returns the most recently computed bid/ask prices
// Returns (bid, ask, ok) where ok indicates if prices are available
func (s *StrategyEngine) GetLatestPrices() (float64, float64, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.latestBid, s.latestAsk, s.hasPrices
}

// ClearPrices invalidates stored prices (called when price feed dies)
func (s *StrategyEngine) ClearPrices() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hasPrices = false
	s.latestBid = 0
	s.latestAsk = 0
	log.Println("[STRATEGY] Prices cleared due to price feed reconnection")
}

