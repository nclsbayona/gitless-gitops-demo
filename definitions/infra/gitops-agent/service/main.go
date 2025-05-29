package main

import (
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"gopkg.in/yaml.v2"
)

// This represents the structure of an individual rule in the YAML file
type sAvoid_Rule struct {
	Regex string           `yaml:"regex"`
	Message string         `yaml:"message"`
}

func (r *sAvoid_Rule) Print() {
	log.Printf(" - Regex: %s\n", r.Regex)
	log.Printf("   Message: %s\n", r.Message)
}

// This represents the structure of the rules YAML file
type Rules struct {
	RepositoryURL string        `yaml:"repository_url"`
	Avoid         []sAvoid_Rule `yaml:"avoid"`
}

func (r *Rules) Print() {
	log.Println("")
	log.Println("--------------------------")
	log.Println("--- GitOps Agent Rules ---")
	log.Printf("Repository URL: %s\n", r.RepositoryURL)
	log.Println("Avoided paths:")
	for _, path := range r.Avoid {
		path.Print()
		log.Println(" ")
	}
	log.Println("--------------------------")
	log.Println("")
}

var rules *Rules
var rules_file string
var ready bool

func init() {
	rules_file = os.Getenv("RULES_FILE")

	if rules_file == "" {
		rules_file = "/rules.yaml"
	}

	parseRulesFile()
}

func parseRulesFile() {

	if rules != nil {
		log.Println("Deleting existing rules...")
		rules = nil
	}

	rules = &Rules{}

	// Read the YAML file
	data, err := os.ReadFile(rules_file)
	if err != nil {
		log.Fatalf("Error reading rules file: %v", err)
	}

	// Parse YAML into Rules struct
	err = yaml.Unmarshal(data, rules)
	if err != nil {
		log.Fatalf("Error parsing YAML: %v", err)
	}

	log.Printf("Loaded %d rules from %s\n", len(rules.Avoid), rules_file)
	rules.Print()
}

func getFileModTime(filename *string) (time.Time, error) {
	info, err := os.Stat(*filename)
	if err != nil {
		return time.Time{}, err
	}
	return info.ModTime(), nil
}

func watchRulesFile(rule_watcher_stop chan bool) {
	// Get initial modification time
	lastModTime, err := getFileModTime(&rules_file)
	if err != nil {
		log.Fatalf("Error getting initial file modification time: %v", err)
	}

	ticker := time.NewTicker(1 * time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-rule_watcher_stop:
			log.Println("Stopping rules file watcher...")
			return
		case <-ticker.C:
			currentModTime, err := getFileModTime(&rules_file)
			if err != nil {
				log.Printf("Error checking file modification time: %v", err)
				continue
			}

			if currentModTime.After(lastModTime) {
				log.Println("Rules file has changed, reloading...")
				parseRulesFile()
				lastModTime = currentModTime
			} else {
				log.Println("No changes detected in rules file")
			}
		}
	}
}

func webhookHandler(w http.ResponseWriter, r *http.Request) {
	log.Println("Received webhook request")

	go func() {
		for !ready {
			log.Println("Waiting for readiness...")
			time.Sleep(10 * time.Second)
		}
		
		ready = false
		log.Println("Working...")

		go func() {
			// Simulate some work
			log.Println("Processing webhook...")
			time.Sleep(1 * time.Minute)
			ready = true
			log.Println("Done")
		}()
	}()

	w.WriteHeader(http.StatusOK)
}

func statusHandler(w http.ResponseWriter, r *http.Request) {
	if ready {
		w.Write([]byte("GitOps Agent is ready!"))
		log.Println("OK")
		w.WriteHeader(http.StatusOK)
	} else {
		w.Write([]byte("GitOps Agent is not ready"))
		log.Println("Not OK")
		w.WriteHeader(http.StatusInternalServerError)
	}
}

func alive(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("GitOps Agent is alive!"))
	log.Println("OK")
}

func startHTTPServer(port string) *http.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/", alive)
	mux.HandleFunc("/webhook", webhookHandler)
	mux.HandleFunc("/status", statusHandler)

	server := &http.Server{
		Addr:    ":" + port,
		Handler: mux,
	}

	go func() {
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("Error starting server: %v", err)
		}
	}()

	return server
}

func main() {
	log.Println("GitOps Agent is running...")
	ready = true

	// Start HTTP server
	port := os.Getenv("PORT")

	server := startHTTPServer(port)
	log.Printf("HTTP server listening on port %s ...\n", port)

	// Channel to signal goroutine to stop
	rule_watcher_stop := make(chan bool)
	// Channel to handle OS signals
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	// Start the file watcher in a goroutine
	go watchRulesFile(rule_watcher_stop)

	// Wait for interrupt signal
	<-sigChan
	log.Println("Received shutdown signal...")

	// Gracefully shutdown the HTTP server
	if err := server.Close(); err != nil {
		log.Printf("Error closing HTTP server: %v", err)
	}

	// Signal the watcher to stop
	rule_watcher_stop <- true
	close(rule_watcher_stop)

	log.Println("GitOps Agent stopped.")
}
