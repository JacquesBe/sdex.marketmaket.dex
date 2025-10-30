package strategy

import (
	"math"

	"github.com/jacquesbecker/sdex-marketmaker/pricefeed"
)

// computeVolatility calculates rolling volatility from log returns
func computeVolatility(window []pricefeed.FeaturesRow) float64 {
	if len(window) < 2 {
		return 0
	}

	// Compute log returns
	returns := make([]float64, 0, len(window)-1)
	for i := 1; i < len(window); i++ {
		r1, r0 := window[i].MidForVol, window[i-1].MidForVol
		if r1 <= 0 || r0 <= 0 {
			continue
		}
		ret := math.Log(r1 / r0)
		returns = append(returns, ret)
	}

	if len(returns) < 2 {
		return 0
	}

	// Compute mean
	mean := 0.0
	for _, r := range returns {
		mean += r
	}
	mean /= float64(len(returns))

	// Compute variance
	varSum := 0.0
	for _, r := range returns {
		varSum += (r - mean) * (r - mean)
	}
	variance := varSum / float64(len(returns)-1)

	return math.Sqrt(variance)
}

// clampPositive ensures value is positive
func clampPositive(x float64) float64 {
	if x <= 0 {
		return 1e-9
	}
	return x
}

// absMax returns the maximum absolute value
func absMax(a, b float64) float64 {
	return math.Max(math.Abs(a), math.Abs(b))
}
