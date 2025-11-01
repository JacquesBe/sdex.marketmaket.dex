# Offers Package

This package contains two main components:

## 1. Offer Monitor (`offer_monitor.go`)

Monitors and tracks your bot's active offers on the Stellar DEX.

## 2. Offer Manager (`offer_manager.go`)

Executes offer creation and updates on the Stellar DEX based on strategy prices.

## Features

- **Hashmap Storage**: Stores current offers using offerID as the key
- **Automatic Reconciliation**: Every `offerMonitorTickRate` ms, fetches current offers and reconciles against local state
- **Smart State Management**:
  - Detects filled offers and removes them from hashmap
  - Detects partial fills and updates quantities
  - Detects new offers and adds them
  - Does nothing if state is unchanged

## Data Structure

### Offer
```go
type Offer struct {
    OfferID  string    // Stellar offer ID
    Quantity string    // Amount of base asset
    Price    string    // Price (counter asset per base asset)
    Type     OfferType // "bid" or "ask"
}
```

## Usage

```go
// Initialize the manager
offerManager := offers.NewManager(botConfig, horizonBaseURI)

// Start monitoring in a goroutine
go func() {
    if err := offerManager.StartMonitor(); err != nil {
        log.Printf("Offer monitor error: %v", err)
    }
}()

// Get current offers (thread-safe)
currentOffers := offerManager.GetOffers()

// Stop the monitor
offerManager.Stop()
```

## Configuration

Set `offerMonitorTickRate` in your MongoDB config (in milliseconds):

```json
{
  "offerManagerOptions": {
    "offerMonitorTickRate": 1500
  }
}
```

## Reconciliation Logic

1. **Fetch** current offers from Horizon API
2. **Filter** to only relevant offers for the trading pair
3. **Update** quantities for existing offers (partial fills)
4. **Add** new offers not in hashmap
5. **Remove** offers no longer in market (filled/cancelled)
6. **Log** state changes

## Thread Safety

- Uses `sync.RWMutex` for concurrent access
- `GetOffers()` returns a deep copy to prevent race conditions

---

# Offer Manager

The offer manager executes offers on the Stellar DEX based on strategy prices.

## Features

- **Grace-based updates**: Only updates offers when price or quantity drift exceeds thresholds
- **Independent bid/ask**: Each side is evaluated and updated independently
- **Retry logic**: Handles `op_offer_not_found` errors by creating new offers
- **Fee control**: Uses `feeMaxStroops` for transaction fee limits

## Configuration

```json
{
  "offerManagerOptions": {
    "offerMonitorTickRate": 1500,
    "offerQuantity": 2,
    "priceGraceBasisPoints": 5,
    "offerGraceQuantity": 1,
    "feeMaxStroops": 100
  }
}
```

- **offerQuantity**: Amount per offer in base asset units
- **priceGraceBasisPoints**: Price tolerance (bps) before updating
- **offerGraceQuantity**: Quantity tolerance before updating
- **feeMaxStroops**: Maximum transaction fee in stroops

## Execution Logic

For each side (bid/ask):

1. **Check if offer exists**
   - No → Create new offer
   - Yes → Evaluate grace conditions

2. **Price Grace Check**
   ```
   priceDiffBps = |currentPrice - newPrice| / currentPrice * 10000
   if priceDiffBps > priceGraceBasisPoints:
       → Update needed
   ```

3. **Quantity Grace Check**
   ```
   qtyDiff = |currentQty - offerQuantity|
   if qtyDiff > offerGraceQuantity:
       → Update needed
   ```

4. **Execute if needed**
   - Use `ManageBuyOffer` for bids
   - Use `ManageSellOffer` for asks
   - Retry with offerID=0 if `op_offer_not_found`

## Usage

```go
// Initialize
monitor := offers.NewMonitor(botConfig, horizonBaseURI)
manager, err := offers.NewManager(botConfig, horizonBaseURI, networkPassphrase, monitor)

// Execute offers (called by strategy)
bidPrice := 0.25
askPrice := 0.26
err = manager.ExecuteOffers(bidPrice, askPrice)
```

## Operations

**Bid (ManageBuyOffer)**:
- Buying: Base Asset (XLM)
- Selling: Counter Asset (EURC)
- Amount: offerQuantity
- Price: Strategy bid price

**Ask (ManageSellOffer)**:
- Selling: Base Asset (XLM)
- Buying: Counter Asset (EURC)
- Amount: offerQuantity
- Price: Strategy ask price
