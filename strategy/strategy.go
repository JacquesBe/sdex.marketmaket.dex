package strategy

import (
	"fmt"
	"log"
	"math"

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
	riskAversion := s.BotConfig.StrategyOptions.RiskAversion
	halfSpreadFloorBps := s.BotConfig.StrategyOptions.HalfSpreadFloor // in basis points
	volatilitySensitivity := s.BotConfig.StrategyOptions.VolatilitySensitivity
	inventoryBias := s.BotConfig.StrategyOptions.InventoryBias
	obImbalanceSensitivity := s.BotConfig.StrategyOptions.OBImbalanceSensitivity

	// 1. Calculate fair price: (1 - blendWeight) * externalMidPrice + blendWeight * microprice
	fairPrice := (1-blendWeight)*features.ExternalMidPrice + blendWeight*features.MicroPrice
	if fairPrice <= 0 {
		log.Printf("[STRATEGY] Invalid fair price: %.6f", fairPrice)
		return 0, 0, false
	}

	// 2. Calculate inventory value: baseAssetQuantity * fairPrice - counterAssetQuantity
	inventoryValue := qBase*fairPrice - qQuote

	// 3. Calculate inventory bias: 0.5 * (riskAversion * rollingVolatility^2) * inventoryValue
	inventoryBiasValue := 0.5 * (riskAversion * rollingVol * rollingVol) * inventoryValue

	// 4. Convert halfSpreadFloor from bps to price using externalMidPrice
	halfSpreadFloor := (halfSpreadFloorBps / 10000.0) * features.ExternalMidPrice

	// 5. Calculate half spread for bid:
	//    halfSpreadFloor + volatilitySensitivity * rollingVolatility + inventoryBias * max(0, inventoryValue) + obImbalanceSensitivity * bidPenalty
	halfSpreadBid := halfSpreadFloor +
		volatilitySensitivity*rollingVol +
		inventoryBias*math.Max(0, inventoryValue) +
		obImbalanceSensitivity*features.BidPenalty

	// 6. Calculate half spread for ask:
	//    halfSpreadFloor + volatilitySensitivity * rollingVolatility + inventoryBias * max(0, -inventoryValue) + obImbalanceSensitivity * askPenalty
	halfSpreadAsk := halfSpreadFloor +
		volatilitySensitivity*rollingVol +
		inventoryBias*math.Max(0, -inventoryValue) +
		obImbalanceSensitivity*features.AskPenalty

	// 7. Calculate final bid and ask prices
	pBid = fairPrice - inventoryBiasValue - halfSpreadBid
	pAsk = fairPrice - inventoryBiasValue + halfSpreadAsk

	// Sanity checks
	if pBid <= 0 || pAsk <= 0 {
		log.Printf("[STRATEGY] Invalid prices: pBid=%.6f pAsk=%.6f", pBid, pAsk)
		return 0, 0, false
	}

	if pBid >= pAsk {
		log.Printf("[STRATEGY] Crossed quotes: pBid=%.6f >= pAsk=%.6f", pBid, pAsk)
		return 0, 0, false
	}

	// Calculate spreads and metrics for logging
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
	log.Printf("   ExtMidPrice: %.6f | MicroPrice: %.6f | RollingVolatility: %.8f", 
		features.ExternalMidPrice, features.MicroPrice, rollingVol)
	log.Printf("   InventoryValue: %.4f | BidPenalty: %.4f | AskPenalty: %.4f", 
		inventoryValue, features.BidPenalty, features.AskPenalty)
	
	// Row 2: Calculated intermediate values
	log.Printf("   FairPrice: %.6f | InventoryBias: %.6f", fairPrice, inventoryBiasValue)
	log.Printf("   HalfSpreadBid: %.6f (%.2f bps) | HalfSpreadAsk: %.6f (%.2f bps)", 
		halfSpreadBid, halfSpreadBidBps, halfSpreadAsk, halfSpreadAskBps)
	
	// Row 3: Final quotes with spreads from external mid
	log.Printf("   🟢 BID: %.6f (%.2f bps from extMid) | 🔴 ASK: %.6f (%.2f bps from extMid)", 
		pBid, bidSpreadFromExtMidBps, pAsk, askSpreadFromExtMidBps)
	log.Printf("   Total Spread: %.6f (%.2f bps)\n", spread, spreadBps)

	return pBid, pAsk, true
}

