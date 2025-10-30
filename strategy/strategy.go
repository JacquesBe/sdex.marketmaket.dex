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

	// Compute fair price r₀ (weighted average of extMid and microprice)
	r0 := (1-s.Config.Eta)*features.ExtMid + s.Config.Eta*features.Microprice
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

	// Compute side-specific imbalance penalties (from TOB)
	Ibid := math.Max(0, -features.TobImbalance) // negative TOB means more asks
	Iask := math.Max(0, features.TobImbalance)  // positive TOB means more bids

	// Compute half-spreads per side (δ_bid and δ_ask)
	deltaBid := s.Config.Delta0 +
		s.Config.LambdaSigma*sigma +
		s.Config.LambdaInv*math.Max(0, V) +
		s.Config.LambdaDepth*Ibid +
		s.Config.LambdaAlpha*0 // alpha not used yet

	deltaAsk := s.Config.Delta0 +
		s.Config.LambdaSigma*sigma +
		s.Config.LambdaInv*math.Max(0, -V) +
		s.Config.LambdaDepth*Iask +
		s.Config.LambdaAlpha*0 // alpha not used yet

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
	log.Printf("   δ_bid=%.6f | δ_ask=%.6f", deltaBid, deltaAsk)
	log.Printf("   🟢 BID: %.6f | 🔴 ASK: %.6f | Spread: %.6f (%.1f bps)", pBid, pAsk, spread, spreadBps)
	log.Printf("   Inventory: XLM=%.2f EURC=%.2f | TOB=%.3f Depth=%.3f\n",
		features.QBase, features.QQuote, features.TobImbalance, features.DepthImbalance)

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
