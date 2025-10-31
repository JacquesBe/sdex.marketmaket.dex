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
	halfSpreadFloor := s.BotConfig.StrategyOptions.HalfSpreadFloor / 10000.0 // convert from bps to absolute
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

	// 4. Calculate half spread for bid:
	//    halfSpreadFloor + volatilitySensitivity * rollingVolatility + inventoryBias * max(0, inventoryValue) + obImbalanceSensitivity * bidPenalty
	halfSpreadBid := halfSpreadFloor +
		volatilitySensitivity*rollingVol +
		inventoryBias*math.Max(0, inventoryValue) +
		obImbalanceSensitivity*features.BidPenalty

	// 5. Calculate half spread for ask:
	//    halfSpreadFloor + volatilitySensitivity * rollingVolatility + inventoryBias * max(0, -inventoryValue) + obImbalanceSensitivity * askPenalty
	halfSpreadAsk := halfSpreadFloor +
		volatilitySensitivity*rollingVol +
		inventoryBias*math.Max(0, -inventoryValue) +
		obImbalanceSensitivity*features.AskPenalty

	// 6. Calculate final bid and ask prices
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

	// Log computed quotes
	spread := pAsk - pBid
	spreadBps := (spread / fairPrice) * 10000
	halfSpreadBidBps := (halfSpreadBid / fairPrice) * 10000
	halfSpreadAskBps := (halfSpreadAsk / fairPrice) * 10000

	log.Printf("\n🎯 [STRATEGY QUOTES]")
	log.Printf("   FairPrice=%.6f | σ=%.8f | InvValue=%.2f | InvBias=%.6f", fairPrice, rollingVol, inventoryValue, inventoryBiasValue)
	log.Printf("   HalfSpread: Bid=%.6f (%.1f bps) | Ask=%.6f (%.1f bps)", halfSpreadBid, halfSpreadBidBps, halfSpreadAsk, halfSpreadAskBps)
	log.Printf("   🟢 BID: %.6f | 🔴 ASK: %.6f | Spread: %.6f (%.1f bps)", pBid, pAsk, spread, spreadBps)
	log.Printf("   Inventory: XLM=%.2f EURC=%.2f | BidPenalty=%.3f AskPenalty=%.3f\n",
		qBase, qQuote, features.BidPenalty, features.AskPenalty)

	return pBid, pAsk, true
}

