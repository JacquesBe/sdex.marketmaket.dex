package strategy

import (
	"log"
	"math"

	"github.com/jacquesbecker/sdex-marketmaker/config"
	"github.com/jacquesbecker/sdex-marketmaker/pricefeed"
)

// StrategyEngine manages the market making strategy
type StrategyEngine struct {
	Config    Config
	Feed      *pricefeed.BinanceFeed
	BotConfig *config.BotConfig
}

// NewStrategyEngine creates a new strategy engine with given config and feed
func NewStrategyEngine(strategyConfig Config, feed *pricefeed.BinanceFeed, botConfig *config.BotConfig) *StrategyEngine {
	// Override VolWindowMs from bot config if available
	if botConfig.StrategyOptions.VolWindowMs > 0 {
		strategyConfig.VolWindowMs = botConfig.StrategyOptions.VolWindowMs
	}
	
	return &StrategyEngine{
		Config:    strategyConfig,
		Feed:      feed,
		BotConfig: botConfig,
	}
}

// ComputeQuotes calculates bid and ask prices using Avellaneda-Stoikov model
// Returns (pBid, pAsk, ok) where ok indicates if quotes are valid
func (s *StrategyEngine) ComputeQuotes() (pBid, pAsk float64, ok bool) {
	// Fetch latest feature snapshot
	features, hasFeatures := s.Feed.GetLatestFeatures()
	if !hasFeatures {
		log.Printf("[STRATEGY] No features available yet")
		return 0, 0, false
	}

	// Get volatility window from bot config (or fallback to strategy config)
	volWindowMs := s.Config.VolWindowMs
	if s.BotConfig != nil && s.BotConfig.StrategyOptions.VolWindowMs > 0 {
		volWindowMs = s.BotConfig.StrategyOptions.VolWindowMs
	}
	
	// Fetch window for volatility computation
	window := s.Feed.GetFeaturesWindow(volWindowMs)
	if len(window) < 2 {
		log.Printf("[STRATEGY] Insufficient window data for volatility (windowMs=%d)", volWindowMs)
		return 0, 0, false
	}

	// Compute rolling volatility σ
	sigma := computeVolatility(window)
	if sigma == 0 {
		sigma = 1e-6 // prevent division by zero
	}

	// Short smoothing window for microprice/TOB/depth
	smoothMs := s.Config.SmoothingMs
	if smoothMs <= 0 {
		smoothMs = 2000
	}
	smallWin := s.Feed.GetFeaturesWindow(smoothMs)

	avg := func(get func(pricefeed.FeaturesRow) float64) float64 {
		if len(smallWin) == 0 {
			return get(features)
		}
		sum := 0.0
		for _, r := range smallWin {
			sum += get(r)
		}
		return sum / float64(len(smallWin))
	}

	// Smoothed fair price inputs
	extMidSm := avg(func(r pricefeed.FeaturesRow) float64 { return r.ExtMid })
	microSm := avg(func(r pricefeed.FeaturesRow) float64 { return r.Microprice })

	// Compute fair price r₀ (weighted average of smoothed extMid and microprice)
	r0 := (1-s.Config.Eta)*extMidSm + s.Config.Eta*microSm
	if r0 <= 0 {
		log.Printf("[STRATEGY] Invalid fair price r0=%.6f", r0)
		return 0, 0, false
	}

	// Compute inventory value V = q_base * r₀ - q_quote
	V := features.QBase*r0 - features.QQuote

	// Check minimum quote size
	if features.QBase < s.Config.MinQuoteSize {
		log.Printf("[STRATEGY] Insufficient base balance: %.2f < %.2f", features.QBase, s.Config.MinQuoteSize)
		return 0, 0, false
	}

	// Compute reservation price (inventory-aware adjustment)
	r := r0 - 0.5*s.Config.Gamma*sigma*sigma*V

	// Compute side-specific imbalance penalties (from smoothed TOB)
	tobSm := avg(func(r pricefeed.FeaturesRow) float64 { return r.TobImbalance })
	Ibid := math.Max(0, -tobSm) // negative TOB means more asks
	Iask := math.Max(0, tobSm)  // positive TOB means more bids

	// Compute depth adjustments (now relative to Delta0, not absolute)
	// This prevents orderbook imbalance from dominating the spread
	depthAdjustBid := s.Config.LambdaDepth * s.Config.Delta0 * Ibid
	depthAdjustAsk := s.Config.LambdaDepth * s.Config.Delta0 * Iask
	
	// Cap depth adjustments to prevent extreme spreads
	maxDepthAdj := s.Config.MaxDepthSkew * s.Config.Delta0
	if maxDepthAdj <= 0 {
		maxDepthAdj = 3.0 * s.Config.Delta0 // fallback: 3x base spread
	}
	if depthAdjustBid > maxDepthAdj {
		depthAdjustBid = maxDepthAdj
	}
	if depthAdjustAsk > maxDepthAdj {
		depthAdjustAsk = maxDepthAdj
	}

	// Inventory adjustment with optional cap in bps
	invAdjBid := s.Config.LambdaInv * math.Max(0, V)
	invAdjAsk := s.Config.LambdaInv * math.Max(0, -V)
	if s.Config.MaxInvAdjBps > 0 {
		cap := (s.Config.MaxInvAdjBps / 10000.0) * r0
		if invAdjBid > cap {
			invAdjBid = cap
		}
		if invAdjAsk > cap {
			invAdjAsk = cap
		}
	}

	// Compute half-spreads per side (δ_bid and δ_ask)
	deltaBid := s.Config.Delta0 +
		s.Config.LambdaSigma*sigma +
		invAdjBid +
		depthAdjustBid +
		s.Config.LambdaAlpha*0 // alpha not used yet

	deltaAsk := s.Config.Delta0 +
		s.Config.LambdaSigma*sigma +
		invAdjAsk +
		depthAdjustAsk +
		s.Config.LambdaAlpha*0 // alpha not used yet

	// Soft clamp: push total spread toward target when inventory is balanced and vol is normal
	if s.Config.TargetSpreadBps > 0 && s.Config.ClampWeight > 0 {
		computed := deltaBid + deltaAsk
		if computed > 0 {
			invFac := 1.0
			if s.Config.InvBalanceThreshold > 0 {
				invFac = math.Max(0, 1.0 - math.Abs(V)/s.Config.InvBalanceThreshold)
			}
			volFac := 1.0
			if s.Config.VolatilityThreshold > 0 {
				volFac = math.Max(0, 1.0 - sigma/s.Config.VolatilityThreshold)
			}
			w := s.Config.ClampWeight * invFac * volFac
			targetAbs := (s.Config.TargetSpreadBps / 10000.0) * r0
			scale := (w*targetAbs + (1-w)*computed) / computed
			// Avoid shrinking below a safety floor of 0.5*Delta0 per side
			floor := math.Max(1e-7, 0.5*s.Config.Delta0/computed)
			if scale < floor {
				scale = floor
			}
			deltaBid *= scale
			deltaAsk *= scale
		}
	}

	// Final bid and ask prices
	pBid = r - deltaBid
	pAsk = r + deltaAsk

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
	spreadBps := (spread / r0) * 10000
	log.Printf("\n🎯 [STRATEGY QUOTES]")
	log.Printf("   r₀=%.6f | r=%.6f | σ=%.5f | V=%.2f EURC", r0, r, sigma, V)
	log.Printf("   δ_bid=%.6f (depth_adj=%.6f) | δ_ask=%.6f (depth_adj=%.6f)", deltaBid, depthAdjustBid, deltaAsk, depthAdjustAsk)
	log.Printf("   🟢 BID: %.6f | 🔴 ASK: %.6f | Spread: %.6f (%.1f bps)", pBid, pAsk, spread, spreadBps)
	log.Printf("   Inventory: XLM=%.2f EURC=%.2f | TOB=%.3f Depth=%.3f\n",
		features.QBase, features.QQuote, tobSm, avg(func(r pricefeed.FeaturesRow) float64 { return r.DepthImbalance }))

	return pBid, pAsk, true
}

// ComputeSpreadDiagnostics returns detailed breakdown of spread components
func (s *StrategyEngine) ComputeSpreadDiagnostics() map[string]float64 {
	features, hasFeatures := s.Feed.GetLatestFeatures()
	if !hasFeatures {
		return map[string]float64{}
	}

	volWindowMs := s.Config.VolWindowMs
	if s.BotConfig != nil && s.BotConfig.StrategyOptions.VolWindowMs > 0 {
		volWindowMs = s.BotConfig.StrategyOptions.VolWindowMs
	}
	
	window := s.Feed.GetFeaturesWindow(volWindowMs)
	sigma := computeVolatility(window)
	if sigma == 0 {
		sigma = 1e-6
	}

	r0 := (1-s.Config.Eta)*features.ExtMid + s.Config.Eta*features.Microprice
	V := features.QBase*r0 - features.QQuote
	riskTerm := 0.5 * s.Config.Gamma * sigma * sigma * V

	delta := s.Config.Delta0 +
		s.Config.LambdaSigma*sigma +
		s.Config.LambdaInv*math.Abs(V) +
		s.Config.LambdaDepth*(math.Abs(features.DepthImbalance)+math.Abs(features.TobImbalance))

	return map[string]float64{
		"r0":             r0,
		"sigma":          sigma,
		"inventory":      V,
		"riskTerm":       riskTerm,
		"delta":          delta,
		"baseDelta":      s.Config.Delta0,
		"sigmaDelta":     s.Config.LambdaSigma * sigma,
		"invDelta":       s.Config.LambdaInv * math.Abs(V),
		"depthDelta":     s.Config.LambdaDepth * (math.Abs(features.DepthImbalance) + math.Abs(features.TobImbalance)),
		"tobImbalance":   features.TobImbalance,
		"depthImbalance": features.DepthImbalance,
		"qBase":          features.QBase,
		"qQuote":         features.QQuote,
	}
}
