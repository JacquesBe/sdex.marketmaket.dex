package strategy

import (
	"fmt"
	"log"

	"github.com/jacquesbecker/sdex-marketmaker/balance"
	"github.com/jacquesbecker/sdex-marketmaker/config"
	"github.com/jacquesbecker/sdex-marketmaker/pricefeed"
)

// StrategyEngine manages the market making strategy
type StrategyEngine struct {
	Feed      *pricefeed.BinanceFeed
	BotConfig *config.BotConfig
}

// NewStrategyEngine creates a new strategy engine with given feed and config
func NewStrategyEngine(feed *pricefeed.BinanceFeed, botConfig *config.BotConfig) *StrategyEngine {
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
		if err == nil {
			if _, err := fmt.Sscanf(baseBalance.Balance, "%f", &qBase); err != nil {
				qBase = 0
			}
			if _, err := fmt.Sscanf(counterBalance.Balance, "%f", &qQuote); err != nil {
				qQuote = 0
			}
		}
	}

	// Get strategy parameters from bot config
	blendWeight := s.BotConfig.StrategyOptions.BlendWeight
	halfSpreadFloorBps := s.BotConfig.StrategyOptions.HalfSpreadFloor // in basis points
	volatilitySensitivity := s.BotConfig.StrategyOptions.VolatilitySensitivity
	inventoryBias := s.BotConfig.StrategyOptions.InventoryBias

	// 1. Calculate fair price: (1 - blendWeight) * externalMidPrice + blendWeight * microprice
	fairPrice := (1-blendWeight)*features.ExternalMidPrice + blendWeight*features.MicroPrice
	if fairPrice <= 0 {
		log.Printf("[STRATEGY] Invalid fair price: %.6f", fairPrice)
		return 0, 0, false
	}

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
	
	// 5. Calculate asymmetric inventory adjustment
	// When long XLM (relativeInventory > 0): widen bid to discourage buying more
	// When short XLM (relativeInventory < 0): widen ask to discourage selling more
	invComponentBid := inventoryBias * max(0, relativeInventory) * features.ExternalMidPrice
	invComponentAsk := inventoryBias * max(0, -relativeInventory) * features.ExternalMidPrice
	
	// Log inventory adjustment calculation
	log.Printf("[INVENTORY ADJ] relativeInventory=%.6f | inventoryBias=%.6f", 
		relativeInventory, inventoryBias)
	log.Printf("[INVENTORY ADJ] invComponentBid=%.8f | invComponentAsk=%.8f", invComponentBid, invComponentAsk)
	
	// 6. Calculate half spreads asymmetrically
	halfSpreadBid := halfSpreadFloor + volComponent + invComponentBid
	halfSpreadAsk := halfSpreadFloor + volComponent + invComponentAsk

	// 7. Calculate final bid and ask prices
	pBid = fairPrice - halfSpreadBid
	pAsk = fairPrice + halfSpreadAsk

	// Calculate spreads and metrics for logging (before sanity checks so we always see the breakdown)
	spread := pAsk - pBid
	spreadBps := (spread / fairPrice) * 10000
	halfSpreadBidBps := (halfSpreadBid / fairPrice) * 10000
	halfSpreadAskBps := (halfSpreadAsk / fairPrice) * 10000
	
	// Calculate bid and ask spread from external mid price in basis points
	bidSpreadFromExtMid := features.ExternalMidPrice - pBid
	bidSpreadFromExtMidBps := (bidSpreadFromExtMid / features.ExternalMidPrice) * 10000
	askSpreadFromExtMid := pAsk - features.ExternalMidPrice
	askSpreadFromExtMidBps := (askSpreadFromExtMid / features.ExternalMidPrice) * 10000

	// Row 1: Input metrics
	log.Printf("\n💰 [STRATEGY INVOCATION]")
	log.Printf("   ExtMidPrice: %.6f | MicroPrice: %.6f | AbsoluteVol: %.8f", 
		features.ExternalMidPrice, features.MicroPrice, rollingVol)
	log.Printf("   RelativeInventory: %.6f (%.1f%%)", relativeInventory, relativeInventory*100)
	
	// Row 2: Calculated intermediate values
	log.Printf("   FairPrice: %.6f | InventoryBias: %.6f", fairPrice, inventoryBias)
	
	// Detailed breakdown of half spread components (asymmetric)
	log.Printf("   📊 [HALF SPREAD BREAKDOWN]")
	log.Printf("      Floor: %.6f (%.2f bps) | Vol: %.6f",
		halfSpreadFloor, halfSpreadFloorBps, volComponent)
	log.Printf("      BID HalfSpread: %.6f (%.2f bps) | ASK HalfSpread: %.6f (%.2f bps)",
		halfSpreadBid, halfSpreadBidBps, halfSpreadAsk, halfSpreadAskBps)
	
	// Row 3: Final quotes with spreads from external mid
	log.Printf("   🟢 BID: %.6f (%.2f bps from extMid) | 🔴 ASK: %.6f (%.2f bps from extMid)", 
		pBid, bidSpreadFromExtMidBps, pAsk, askSpreadFromExtMidBps)
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

	return pBid, pAsk, true
}

