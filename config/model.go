package config

import "go.mongodb.org/mongo-driver/bson/primitive"

// BotConfig represents the complete bot configuration stored in MongoDB
// This matches the structure of your MongoDB document
type BotConfig struct {
	ID                    primitive.ObjectID     `bson:"_id"`
	PublicKey             string                 `bson:"publicKey"`
	SecretKey             string                 `bson:"secretKey"`
	BaseAsset             string                 `bson:"baseAsset"`
	CounterAsset          string                 `bson:"counterAsset"`
	BaseAssetIssuer       string                 `bson:"baseAssetIssuer"`
	CounterAssetIssuer    string                 `bson:"counterAssetIssuer"`
	PriceFeedOptions      PriceFeedOptions       `bson:"priceFeedOptions"`
	BalanceMonitorOptions BalanceMonitorOptions  `bson:"balanceMonitorOptions"`
	StrategyOptions       StrategyOptions        `bson:"strategyOptions"`
	OfferManagerOptions   OfferManagerOptions    `bson:"offerManagerOptions"`
}

// PriceFeedOptions contains configuration for the price feed source (CEX)
type PriceFeedOptions struct {
	BaseAsset          string  `bson:"baseAsset"`
	CounterAsset       string  `bson:"counterAsset"`
	Inverse            bool    `bson:"inverse"`
	PriceFeedMultiple  float64 `bson:"priceFeedMultiple"`
	BufferLength       int     `bson:"bufferLength"`
	BookDepth          int     `bson:"bookDepth"` // Order book depth level (5, 10, or 20)
}

// GetTradingSymbol returns the Binance trading symbol format (e.g., "XLMEUR")
func (p *PriceFeedOptions) GetTradingSymbol() string {
	return p.BaseAsset + p.CounterAsset
}

// BalanceMonitorOptions contains configuration for the balance monitor service
type BalanceMonitorOptions struct {
	TickRate int `bson:"tickRate"` // Tick rate in milliseconds
}

// StrategyOptions contains configuration for the strategy engine
type StrategyOptions struct {
	VolWindowMs              int64   `bson:"volWindowMs"`              // Volatility window in milliseconds
	BlendWeight              float64 `bson:"blendWeight"`              // Weight for microprice in fair price calculation
	RiskAversion             float64 `bson:"riskAversion"`             // Risk aversion parameter
	HalfSpreadFloor          float64 `bson:"halfSpreadFloor"`          // Base half spread floor (in bps)
	VolatilitySensitivity    float64 `bson:"volatilitySensitivity"`    // Volatility sensitivity multiplier
	InventoryBias            float64 `bson:"inventoryBias"`            // Inventory bias coefficient
	OBImbalanceSensitivity   float64 `bson:"obImbalanceSensitivity"`   // Order book imbalance sensitivity
}

// OfferManagerOptions contains configuration for the offer manager service
type OfferManagerOptions struct {
	OfferMonitorTickRate     int     `bson:"offerMonitorTickRate"`     // Tick rate in milliseconds
	OfferQuantity            float64 `bson:"offerQuantity"`            // Amount per offer in base asset units
	PriceGraceBasisPoints    float64 `bson:"priceGraceBasisPoints"`    // Price tolerance in basis points
	OfferGraceQuantity       float64 `bson:"offerGraceQuantity"`       // Quantity tolerance in base asset units
	FeeMaxStroops            int64   `bson:"feeMaxStroops"`            // Maximum transaction fee in stroops
}
