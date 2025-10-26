package config

import (
	"context"
	"fmt"
	"log"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// MongoService handles MongoDB connections and configuration retrieval
type MongoService struct {
	client   *mongo.Client
	database *mongo.Database
	config   *BotConfig
}

// NewMongoService creates a new MongoDB service and connects to the database
// connectionString: MongoDB connection URI (e.g., "mongodb://localhost:27017")
// databaseName: Name of the database containing bot configs
func NewMongoService(connectionString, databaseName string) (*MongoService, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Connect to MongoDB
	client, err := mongo.Connect(ctx, options.Client().ApplyURI(connectionString))
	if err != nil {
		return nil, fmt.Errorf("failed to connect to MongoDB: %w", err)
	}

	// Verify connection with a ping
	if err := client.Ping(ctx, nil); err != nil {
		return nil, fmt.Errorf("failed to ping MongoDB: %w", err)
	}

	log.Println("Successfully connected to MongoDB")

	return &MongoService{
		client:   client,
		database: client.Database(databaseName),
	}, nil
}

// LoadConfig loads the bot configuration from MongoDB using the provided document ID
// configID: The ObjectID of the configuration document (from env variable)
func (m *MongoService) LoadConfig(configID string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Convert string ID to MongoDB ObjectID
	objectID, err := primitive.ObjectIDFromHex(configID)
	if err != nil {
		return fmt.Errorf("invalid config ID format: %w", err)
	}

	// Query the config collection for the document with matching _id
	collection := m.database.Collection("reference.data.marketMakerConfig")
	var config BotConfig

	filter := bson.M{"_id": objectID}
	err = collection.FindOne(ctx, filter).Decode(&config)
	if err != nil {
		return fmt.Errorf("failed to load config from MongoDB: %w", err)
	}

	m.config = &config
	log.Printf("Loaded config for %s/%s pair", config.BaseAsset, config.CounterAsset)

	return nil
}

// GetConfig returns the currently loaded bot configuration
// This is used by all layers to access configuration data
func (m *MongoService) GetConfig() *BotConfig {
	return m.config
}

// Close closes the MongoDB connection
func (m *MongoService) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return m.client.Disconnect(ctx)
}
