package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/go-redis/redis/v8"
	"googlemaps.github.io/maps"
)

type Configuration struct {
	RedisUrl   string
	MapsApiKey string
}

var redisClient *redis.Client
var mapsClient *maps.Client

type Location struct {
	OrderID string  `json:"order_id"`
	Lat     float64 `json:"lat"`
	Lng     float64 `json:"lng"`
}

type Transport struct {
	OrderID string `json:"order_id"`
	Mode    string `json:"mode"`
}

func main() {

	file, _ := os.Open("conf.json")
	defer file.Close()
	decoder := json.NewDecoder(file)
	conf := Configuration{}
	errF := decoder.Decode(&conf)
	if errF != nil {
		fmt.Println("error:", errF)
	}
	// Initialize Redis client
	opt, _ := redis.ParseURL(conf.RedisUrl)
	redisClient = redis.NewClient(opt)

	// Initialize Google Maps client
	var err error
	mapsClient, err = maps.NewClient(maps.WithAPIKey(conf.MapsApiKey))
	if err != nil {
		log.Fatalf("Failed to create Google Maps client: %v", err)
	}

	// Define routes
	http.HandleFunc("/location/current", handleCurrentLocation)
	http.HandleFunc("/location/target", handleTargetLocation)
	http.HandleFunc("/transport", handleTransport)

	// Start the server
	log.Println("Server listening on port 8080")
	log.Fatal(http.ListenAndServe(":8080", nil))
}

func handleTransport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var transport Transport
	err := json.NewDecoder(r.Body).Decode(&transport)
	if err != nil {
		http.Error(w, "Invalid request payload", http.StatusBadRequest)
		return
	}

	err = updateMode(transport)
	if err != nil {
		http.Error(w, "Failed to update and calculate time", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
}

func handleCurrentLocation(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var location Location
	err := json.NewDecoder(r.Body).Decode(&location)
	if err != nil {
		http.Error(w, "Invalid request payload", http.StatusBadRequest)
		return
	}

	travelTime, err := updateAndCalculateTime(location, "current")
	if err != nil {
		http.Error(w, "Failed to update and calculate time", http.StatusInternalServerError)
		return
	}

	err = publishTravelTime(location.OrderID, travelTime)
	if err != nil {
		http.Error(w, "Failed to publish travel time", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, travelTime)
}

func handleTargetLocation(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var location Location
	err := json.NewDecoder(r.Body).Decode(&location)
	if err != nil {
		http.Error(w, "Invalid request payload", http.StatusBadRequest)
		return
	}

	travelTime, err := updateAndCalculateTime(location, "target")
	if err != nil {
		http.Error(w, "Failed to update and calculate time", http.StatusInternalServerError)
		return
	}

	if travelTime > 0 {
		err = publishTravelTime(location.OrderID, travelTime)
		if err != nil {
			http.Error(w, "Failed to publish travel time", http.StatusInternalServerError)
			return
		}
	}

	w.WriteHeader(http.StatusOK)
	fmt.Fprint(w, travelTime)
}

func updateAndCalculateTime(location Location, locationType string) (time.Duration, error) {
	log.Println("Running update and calculate")
	ctx := context.Background()

	// Update Redis with the new location information
	err := redisClient.HSet(ctx, location.OrderID, locationType, fmt.Sprintf("%f,%f", location.Lat, location.Lng)).Err()
	if err != nil {
		log.Println("failed to update location in Redis:")
		return 0, fmt.Errorf("failed to update location in Redis: %v", err)
	}

	// Retrieve current and target locations from Redis
	currentLoc, err := redisClient.HGet(ctx, location.OrderID, "current").Result()
	if err != nil {
		log.Println("failed to get current location from Redis")
		return 0, fmt.Errorf("failed to get current location from Redis: %v", err)
	}

	targetLoc, err := redisClient.HGet(ctx, location.OrderID, "target").Result()
	if err != nil {
		log.Println("failed to get target location from Redis")
		return 0, fmt.Errorf("failed to get target location from Redis: %v", err)
	}

	mode, err := redisClient.HGet(ctx, location.OrderID, "mode").Result()
	if err != nil {
		log.Println("failed to get travel mode from Redis")
		mode = "walking"
	}

	// Calculate travel time using Google Maps API
	travelTime, err := calculateTravelTime(currentLoc, targetLoc, mode)
	if err != nil {
		log.Println("failed to calculate travel time")
		return 0, fmt.Errorf("failed to calculate travel time: %v", err)
	}

	return travelTime, nil
}

func updateMode(transport Transport) error {
	ctx := context.Background()

	// Update Redis with the new location information
	err := redisClient.HSet(ctx, transport.OrderID, "mode", transport.Mode).Err()
	if err != nil {
		log.Println("failed to update mode in Redis")
		return fmt.Errorf("failed to update mode in Redis: %v", err)
	}

	return nil
}

func calculateTravelTime(currentLoc, targetLoc, mode string) (time.Duration, error) {
	// Parse current and target locations
	var current, target maps.LatLng
	_, err := fmt.Sscanf(currentLoc, "%f,%f", &current.Lat, &current.Lng)
	if err != nil {
		log.Println("failed to parse current location")
		return 0, fmt.Errorf("failed to parse current location: %v", err)
	}
	_, err = fmt.Sscanf(targetLoc, "%f,%f", &target.Lat, &target.Lng)
	if err != nil {
		log.Println("failed to parse target location")
		return 0, fmt.Errorf("failed to parse target location: %v", err)
	}

	// Calculate travel time using Google Maps API
	routes, _, err := mapsClient.Directions(context.Background(), &maps.DirectionsRequest{
		Origin:      current.String(),
		Destination: target.String(),
		Mode:        maps.Mode(mode),
	})
	if err != nil {
		log.Printf("failed to get directions: %v", err)
		return 0, fmt.Errorf("failed to get directions: %v", err)
	}

	if len(routes) == 0 || len(routes[0].Legs) == 0 {
		log.Printf("no directions found: %v", routes)
		return 0, fmt.Errorf("no directions found")
	}
	return routes[0].Legs[0].Duration, nil
}

func publishTravelTime(orderID string, travelTime time.Duration) error {
	// Implement the logic to publish travel time to another microservice
	// Can consider using message queues or HTTP requests  or maybe even websocket
	log.Printf("Publishing travel time for order %s: %v", orderID, travelTime)
	return nil
}
