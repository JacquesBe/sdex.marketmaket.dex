package pricefeed

// FeatureUpdateCallback is called when new features are available
type FeatureUpdateCallback func()

// DisconnectCallback is called when the price feed disconnects
type DisconnectCallback func()

// PriceFeed defines the interface that all price feed implementations must satisfy
type PriceFeed interface {
	// Start begins the price feed service (blocking)
	Start() error
	
	// Stop gracefully stops the price feed
	Stop()
	
	// GetLatestFeatures returns the most recent feature row
	GetLatestFeatures() (FeaturesRow, bool)
	
	// GetLastMessageAt returns the last time a message was received (unix ms)
	GetLastMessageAt() int64
	
	// SetFeatureUpdateCallback sets the callback to invoke when features are updated
	SetFeatureUpdateCallback(callback FeatureUpdateCallback)
	
	// SetDisconnectCallback sets the callback to invoke when feed disconnects
	SetDisconnectCallback(callback DisconnectCallback)
}
