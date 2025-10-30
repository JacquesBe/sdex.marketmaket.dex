package strategy

// Config holds all tunable strategy parameters
type Config struct {
	Gamma        float64 // risk aversion parameter
	Eta          float64 // weighting factor between extMid and microprice
	Delta0       float64 // base spread in quote terms
	LambdaSigma  float64 // spread scaling factor by volatility
	LambdaInv    float64 // inventory bias coefficient
	LambdaDepth  float64 // orderbook imbalance coefficient
	LambdaAlpha  float64 // optional alpha factor coefficient
	VolWindowMs  int64   // rolling window in ms for volatility
	MinQuoteSize float64 // minimum order size (in base)
}

// Default parameter values
const (
	DefaultGamma       = 0.05
	DefaultEta         = 0.5
	DefaultDelta0      = 0.001
	DefaultLambdaSigma = 1.2
	DefaultLambdaInv   = 0.5
	DefaultLambdaDepth = 0.3
	DefaultLambdaAlpha = 0.0
	DefaultVolWindowMs = 60000
	DefaultMinQuoteSize = 10.0
)

// NewDefaultConfig returns a Config with sensible defaults
func NewDefaultConfig() Config {
	return Config{
		Gamma:        DefaultGamma,
		Eta:          DefaultEta,
		Delta0:       DefaultDelta0,
		LambdaSigma:  DefaultLambdaSigma,
		LambdaInv:    DefaultLambdaInv,
		LambdaDepth:  DefaultLambdaDepth,
		LambdaAlpha:  DefaultLambdaAlpha,
		VolWindowMs:  DefaultVolWindowMs,
		MinQuoteSize: DefaultMinQuoteSize,
	}
}
