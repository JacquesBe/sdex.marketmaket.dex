package config

import "go.mongodb.org/mongo-driver/bson/primitive"

// BotConfig represents the complete bot configuration stored in MongoDB
// This matches the structure of your MongoDB document
type BotConfig struct {
	ID                 primitive.ObjectID `bson:"_id"`
	BaseAsset          string             `bson:"baseAsset"`
	CounterAsset       string             `bson:"counterAsset"`
	BaseAssetIssuer    string             `bson:"baseAssetIssuer"`
	CounterAssetIssuer string             `bson:"counterAssetIssuer"`
	PriceFeedOptions   PriceFeedOptions   `bson:"priceFeedOptions"`
}

// PriceFeedOptions contains configuration for the price feed source (CEX)
type PriceFeedOptions struct {
	BaseAsset          string  `bson:"baseAsset"`
	CounterAsset       string  `bson:"counterAsset"`
	Inverse            bool    `bson:"inverse"`
	PriceFeedMultiple  float64 `bson:"priceFeedMultiple"`
}

// GetTradingSymbol returns the Binance trading symbol format (e.g., "XLMEUR")
func (p *PriceFeedOptions) GetTradingSymbol() string {
	return p.BaseAsset + p.CounterAsset
}
