package strategy

// Config holds all tunable strategy parameters
type Config struct {
	Gamma                float64 // risk aversion parameter
	Eta                  float64 // weighting factor between extMid and microprice
	Delta0               float64 // base spread in quote terms (absolute, not bps)
	LambdaSigma          float64 // spread scaling factor by volatility
	LambdaInv            float64 // inventory bias coefficient (absolute price per unit of inventory value)
	LambdaDepth          float64 // orderbook imbalance coefficient (as multiplier of Delta0)
	LambdaAlpha          float64 // optional alpha factor coefficient
	VolWindowMs          int64   // rolling window in ms for volatility
	MinQuoteSize         float64 // minimum order size (in base)
	MaxDepthSkew         float64 // max spread adjustment from depth imbalance (as multiplier of Delta0)
	SmoothingMs          int64   // window for smoothing TOB/depth/microprice (ms)
	MaxInvAdjBps         float64 // cap for inventory-induced half-spread per side (in bps), 0 disables cap
	TargetSpreadBps      float64 // desired total spread in bps under normal conditions
	ClampWeight          float64 // strength of soft clamp toward target spread [0,1]
	InvBalanceThreshold  float64 // |V| threshold (quote units) below which we consider inventory balanced
	VolatilityThreshold  float64 // sigma threshold below which we consider volatility "normal"
}

// Default parameter values
const (
	DefaultGamma               = 0.05
	DefaultEta                 = 0.30
	DefaultDelta0              = 0.00028 // Slightly tighter base half-spread
	DefaultLambdaSigma         = 0.25    // Softer volatility impact
	DefaultLambdaInv           = 0.004   // Smaller inventory impact
	DefaultLambdaDepth         = 0.30    // Softer depth skew
	DefaultLambdaAlpha         = 0.0
	DefaultVolWindowMs         = 60000
	DefaultMinQuoteSize        = 10.0
	DefaultMaxDepthSkew        = 2.0     // Tighter cap on depth-driven widening
	DefaultSmoothingMs         = 2500    // 2.5s smoothing for TOB/depth/microprice
	DefaultMaxInvAdjBps        = 8.0     // Cap inventory adjustment to 8 bps per side
	DefaultTargetSpreadBps     = 25.0    // Aim for ~28 bps under normal conditions
	DefaultClampWeight         = 0.6     // Moderately strong clamp when conditions normal
	DefaultInvBalanceThreshold = 1.0     // ~1 quote unit considered balanced
	DefaultVolatilityThreshold = 0.0002  // "Normal" sigma threshold
)

// NewDefaultConfig returns a Config with sensible defaults
func NewDefaultConfig() Config {
	return Config{
		Gamma:               DefaultGamma,
		Eta:                 DefaultEta,
		Delta0:              DefaultDelta0,
		LambdaSigma:         DefaultLambdaSigma,
		LambdaInv:           DefaultLambdaInv,
		LambdaDepth:         DefaultLambdaDepth,
		LambdaAlpha:         DefaultLambdaAlpha,
		VolWindowMs:         DefaultVolWindowMs,
		MinQuoteSize:        DefaultMinQuoteSize,
		MaxDepthSkew:        DefaultMaxDepthSkew,
		SmoothingMs:         DefaultSmoothingMs,
		MaxInvAdjBps:        DefaultMaxInvAdjBps,
		TargetSpreadBps:     DefaultTargetSpreadBps,
		ClampWeight:         DefaultClampWeight,
		InvBalanceThreshold: DefaultInvBalanceThreshold,
		VolatilityThreshold: DefaultVolatilityThreshold,
	}
}
