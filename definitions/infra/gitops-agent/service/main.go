package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"sync"
	"syscall"
	"time"

	"gopkg.in/yaml.v2"
)

// OCIRepositoryInfo represents information about a repository
type OCIRepositoryInfo struct {
	LastUpdated time.Time
	Tags        []string
}

// HistoryEntry represents a single task in the history
type HistoryEntry struct {
	Timestamp time.Time `json:"timestamp"`
	Action    string    `json:"action"`
	Details   string    `json:"details"`
}

// TaskHistory manages the history of tasks
type TaskHistory struct {
	entries []HistoryEntry
	mutex   sync.RWMutex
}

func (h *TaskHistory) Add(action, details string) {
	h.mutex.Lock()
	defer h.mutex.Unlock()
	h.entries = append(h.entries, HistoryEntry{
		Timestamp: time.Now(),
		Action:    action,
		Details:   details,
	})
}

func (h *TaskHistory) GetAll() []HistoryEntry {
	h.mutex.RLock()
	defer h.mutex.RUnlock()
	return h.entries
}

var (
	rules           *Rules
	rules_file      string
	ready           bool
	lastRepoCheck   time.Time
	currentRepoInfo OCIRepositoryInfo
	history         TaskHistory
)

// This represents the structure of an individual rule in the YAML file
type sOnly_Rule struct {
	Regex   string `yaml:"regex"`
	Message string `yaml:"message"`
	matcher *regexp.Regexp
}

func (r *sOnly_Rule) Print() {
	log.Printf(" - Regex: %s\n", r.Regex)
	log.Printf("   Message: %s\n", r.Message)
}

func (r *sOnly_Rule) Compile() error {
	var err error
	r.matcher, err = regexp.Compile(r.Regex)
	return err
}

func (r *sOnly_Rule) Matches(tag string) bool {
	if r.matcher == nil {
		if err := r.Compile(); err != nil {
			log.Printf("Error compiling regex: %v", err)
			return false
		}
	}
	return r.matcher.MatchString(tag)
}

// This represents the structure of the rules YAML file
type Rules struct {
	RepositoryURL string     `yaml:"repository_url"`
	Only          sOnly_Rule `yaml:"only"`
}

func (r *Rules) Print() {
	log.Println("")
	log.Println("--------------------------")
	log.Println("--- GitOps Agent Rules ---")
	log.Printf("Repository URL: %s\n", r.RepositoryURL)
	log.Println("Rules:")
	r.Only.Print()
	log.Println("--------------------------")
	log.Println("")
}

func (r *Rules) CheckRepository() (bool, error) {
	// Split RepositoryURL into registry and repository
	parts := regexp.MustCompile(`^([^/]+)/(.+)$`).FindStringSubmatch(r.RepositoryURL)
	if len(parts) != 3 {
		return false, fmt.Errorf("invalid repository URL format: %s", r.RepositoryURL)
	}
	registry := parts[1]
	repository := parts[2]
	url := fmt.Sprintf("http://%s/v2/%s/tags/list", registry, repository)
	resp, err := http.Get(url)
	if err != nil {
		return false, fmt.Errorf("error checking repository: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return false, fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return false, fmt.Errorf("error reading response: %v", err)
	}

	var tagsResponse struct {
		Name string   `json:"name"`
		Tags []string `json:"tags"`
	}

	if err := json.Unmarshal(body, &tagsResponse); err != nil {
		return false, fmt.Errorf("error parsing JSON: %v", err)
	}

	// Check if any new tag matches our rules
	hasChanges := false
	if lastRepoCheck.IsZero() {
		hasChanges = true
	} else {
		for _, tag := range tagsResponse.Tags {
			if r.Only.Matches(tag) {
				// Check if this tag wasn't in our previous check
				found := false
				for _, oldTag := range currentRepoInfo.Tags {
					if oldTag == tag {
						found = true
						break
					}
				}
				if !found {
					hasChanges = true
					break
				}
			}
		}
	}

	// Update current info
	currentRepoInfo.Tags = tagsResponse.Tags
	currentRepoInfo.LastUpdated = time.Now()
	lastRepoCheck = time.Now()

	history.Add("CheckRepository", fmt.Sprintf("Checked repository %s for changes. Found changes: %v", r.RepositoryURL, hasChanges))
	return hasChanges, nil
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

			// Check repository changes
			if rules != nil {
				changed, err := rules.CheckRepository()
				if err != nil {
					log.Printf("Error checking repository: %v", err)
				} else if changed {
					log.Println("Repository changes detected!")
					// Process repository changes
					processRepositoryChanges()
				} else {
					log.Println("No repository changes detected")
				}
			}
		}
	}
}

func processRepositoryChanges() {
	log.Printf("Processing changes in repository %s", rules.RepositoryURL)
	matchedTags := []string{}
	for _, tag := range currentRepoInfo.Tags {
		if rules.Only.Matches(tag) {
			log.Printf("Tag %s matches rule pattern %s", tag, rules.Only.Regex)
			log.Printf("Rule message: %s", rules.Only.Message)
			matchedTags = append(matchedTags, tag)
		}
	}
	if len(matchedTags) > 0 {
		history.Add("ProcessChanges", fmt.Sprintf("Processed changes in repository %s. Matched tags: %v", rules.RepositoryURL, matchedTags))
	}
}

func webhookHandler(w http.ResponseWriter, r *http.Request) {
	log.Println("Received webhook request")
	history.Add("WebhookReceived", "Processing webhook request")

	go func() {
		for !ready {
			log.Println("Waiting for readiness...")
			time.Sleep(10 * time.Second)
		}

		ready = false

		go func() {
			log.Println("Processing webhook...")
			history.Add("WebhookProcessing", "Processing webhook request")
			// Check repository for changes
			if rules != nil {
				changed, err := rules.CheckRepository()
				if err != nil {
					log.Printf("Error checking repository: %v", err)
				} else if changed {
					log.Println("Repository changes detected!")
					processRepositoryChanges()
				} else {
					log.Println("No repository changes detected")
				}
			}

			ready = true
			log.Println("Done")
		}()
	}()

	w.WriteHeader(http.StatusOK)
}

func statusHandler(w http.ResponseWriter, r *http.Request) {
	if ready {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("GitOps Agent is ready!"))
	} else {
		w.WriteHeader(http.StatusInternalServerError)
		w.Write([]byte("GitOps Agent is not ready"))
	}
}

func alive(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("GitOps Agent is alive!"))
}

func historyHandler(w http.ResponseWriter, r *http.Request) {
	entries := history.GetAll()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(entries)
}

func startHTTPServer(port string) *http.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/", alive)
	mux.HandleFunc("/webhook", webhookHandler)
	mux.HandleFunc("/status", statusHandler)
	mux.HandleFunc("/history", historyHandler)

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

func init() {
	// Initialize the history slice
	history = TaskHistory{
		entries: make([]HistoryEntry, 0),
	}

	rules_file = os.Getenv("RULES_FILE")

	if rules_file == "" {
		rules_file = "/etc/agent/rules.yaml"
	}

	parseRulesFile()
	history.Add("Startup", "Agent initialized with rules file: "+rules_file)

	// Initialize repository monitoring
	if rules != nil {
		// Compile the regex pattern
		if err := rules.Only.Compile(); err != nil {
			log.Printf("Warning: Error compiling regex pattern: %v", err)
		}

		// Do initial repository check
		changed, err := rules.CheckRepository()
		if err != nil {
			log.Printf("Warning: Error in initial repository check: %v", err)
		} else if changed {
			log.Println("Initial repository state recorded")
		}
	}
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
