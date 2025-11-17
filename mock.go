package main

import (
	"encoding/json"
	"flag"
	"log"
	"net/http"
	"strconv"
	"sync"
)

type ConfigPayload struct {
	SmallBid       float64 `json:"small_bid"`
	LargeBid       float64 `json:"large_bid"`
	MaxConcurrency int     `json:"max_concurrency"`
}

var (
	mu   sync.RWMutex
	data = ConfigPayload{
		SmallBid:       0.1,
		LargeBid:       0.5,
		MaxConcurrency: 10,
	}
)

func main() {
	addr := flag.String("addr", ":8080", "Address to listen on")
	flag.Parse()

	http.HandleFunc("/config", handleConfig)
	http.HandleFunc("/set", handleSet)

	log.Printf("Mock server listening on %s", *addr)
	log.Fatal(http.ListenAndServe(*addr, nil))
}

func handleConfig(w http.ResponseWriter, r *http.Request) {
	mu.RLock()
	defer mu.RUnlock()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(data)
}

func handleSet(w http.ResponseWriter, r *http.Request) {
	mu.Lock()
	defer mu.Unlock()

	q := r.URL.Query()

	if s := q.Get("small_bid"); s != "" {
		if v, err := strconv.ParseFloat(s, 64); err == nil {
			data.SmallBid = v
		}
	}
	if s := q.Get("large_bid"); s != "" {
		if v, err := strconv.ParseFloat(s, 64); err == nil {
			data.LargeBid = v
		}
	}
	if s := q.Get("max_concurrency"); s != "" {
		if v, err := strconv.Atoi(s); err == nil {
			data.MaxConcurrency = v
		}
	}

	log.Printf("Updated mock config: %+v", data)
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(data)
}
