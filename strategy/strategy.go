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
	ewmaPrice      float64  // Exponentially weighted moving average price
	ewmaInitialized bool    // Whether EWMA has been initialized
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

	// 1. Calculate instantaneous fair price: (1 - blendWeight) * externalMidPrice + blendWeight * microprice
	instantPrice := (1-blendWeight)*features.ExternalMidPrice + blendWeight*features.MicroPrice
	if instantPrice <= 0 {
		log.Printf("[STRATEGY] Invalid instant price: %.6f", instantPrice)
		return 0, 0, false
	}
	
	// Apply EWMA smoothing to prevent price manipulation
	// alpha controls speed: 0.1 = very slow (90% old), 0.5 = balanced, 1.0 = no smoothing
	alpha := s.BotConfig.StrategyOptions.PriceSmoothing
	if alpha <= 0 || alpha > 1 {
		alpha = 0.2 // Default if not set or invalid
	}
	s.mu.Lock()
	if !s.ewmaInitialized {
		s.ewmaPrice = instantPrice
		s.ewmaInitialized = true
	} else {
		s.ewmaPrice = alpha*instantPrice + (1-alpha)*s.ewmaPrice
	}
	fairPrice := s.ewmaPrice
	s.mu.Unlock()
	
	log.Printf("[PRICE SMOOTHING] Instant: %.6f | EWMA: %.6f | Diff: %.6f (%.2f bps)",
		instantPrice, fairPrice, instantPrice-fairPrice, ((instantPrice-fairPrice)/fairPrice)*10000)
	
	// 2. Calculate relative inventory: (qBase * ExtMid) / ((qBase * ExtMid) + qQuote) - 0.5
	// Range: [-0.5, 0.5] where 0 = balanced
	baseValue := qBase * features.ExternalMidPrice
	totalValue := baseValue + qQuote
	var relativeInventory float64
	if totalValue > 0 {
		relativeInventory = (baseValue / totalValue) - 0.5
	}
	
	// Calculate total wallet value in base asset terms
	quoteInBase := qQuote / fairPrice
	totalWalletBase := qBase + quoteInBase

	// 3. Convert halfSpreadFloor from bps to price
	halfSpreadFloor := (halfSpreadFloorBps / 10000.0) * features.ExternalMidPrice
	
	// 4. Calculate volatility component (already in price units)
	volComponent := volatilitySensitivity * rollingVol
	
	// 5. Calculate asymmetric inventory adjustment
	// When long XLM (relativeInventory > 0): widen bid to discourage buying more
	// When short XLM (relativeInventory < 0): widen ask to discourage selling more
	invComponentBid := inventoryBias * max(0, relativeInventory) * fairPrice
	invComponentAsk := inventoryBias * max(0, -relativeInventory) * fairPrice
	
	// 6. Calculate orderbook imbalance penalties
	// BidPenalty high = orderbook ask side heavy (sellers) -> widen YOUR bid (you compete with sellers)
	// AskPenalty high = orderbook bid side heavy (buyers) -> widen YOUR ask (you compete with buyers)
	obPenaltyBid := orderBookBias * features.BidPenalty * fairPrice
	obPenaltyAsk := orderBookBias * features.AskPenalty * fairPrice
	
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
	volComponentBps := (volComponent / fairPrice) * 10000
	
	// Calculate half spreads in base asset terms
	halfSpreadBidBaseAsset := halfSpreadBid / fairPrice * totalWalletBase
	halfSpreadAskBaseAsset := halfSpreadAsk / fairPrice * totalWalletBase
	
	log.Printf("\n" +
		"╔════════════════════════════════════════════════════════════════════════════════╗\n" +
		"║ 💰 MARKET MAKING STRATEGY                                                     ║\n" +
		"╠════════════════════════════════════════════════════════════════════════════════╣\n" +
		"║ Price    │ Instant: %8.4f │ EWMA: %8.4f │ Final: %8.4f              ║\n" +
		"║ External │ Bid: %8.4f │ Ask: %8.4f │ Mid: %8.4f                ║\n" +
		"║ Portfolio│ Inventory: %6.1f%% │ Total Value: %8.2f %-4s                   ║\n" +
		"║ Volatility│ Rolling: %8.6f │ Impact: %6.1f bps each side             ║\n" +
		"╠════════════════════════════════════════════════════════════════════════════════╣\n" +
		"║          │    PRICE    │  HALF SPREAD  │  FROM FAIR  │  IN BASE ASSET         ║\n" +
		"║ 🟢 BID   │  %9.6f │   %6.1f bps   │  %6.1f bps  │  %8.2f %-4s      ║\n" +
		"║ 🔴 ASK   │  %9.6f │   %6.1f bps   │  %6.1f bps  │  %8.2f %-4s      ║\n" +
		"║ SPREAD   │  %9.6f │   %6.1f bps   │             │                        ║\n" +
		"╚════════════════════════════════════════════════════════════════════════════════╝",
		instantPrice, fairPrice, fairPrice,
		features.BestBid, features.BestAsk, features.ExternalMidPrice,
		relativeInventory*100, totalWalletBase, s.BotConfig.BaseAsset,
		rollingVol, volComponentBps,
		pBid, halfSpreadBidBps, halfSpreadBidBps, halfSpreadBidBaseAsset, s.BotConfig.BaseAsset,
		pAsk, halfSpreadAskBps, halfSpreadAskBps, halfSpreadAskBaseAsset, s.BotConfig.BaseAsset,
		spread, spreadBps)
	
	if invComponentBid > 0.001 || invComponentAsk > 0.001 || obPenaltyBid > 0.001 || obPenaltyAsk > 0.001 {
		log.Printf("   ⚡ Adjustments: Inv(%.1f/%.1f) OB(%.1f/%.1f) bps",
			(invComponentBid/fairPrice)*10000, (invComponentAsk/fairPrice)*10000,
			(obPenaltyBid/fairPrice)*10000, (obPenaltyAsk/fairPrice)*10000)
	}

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

