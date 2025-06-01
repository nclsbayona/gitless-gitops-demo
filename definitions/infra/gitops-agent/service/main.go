package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/sigstore/cosign/v2/pkg/cosign"
	"github.com/sigstore/cosign/v2/pkg/oci/remote"
	"github.com/sigstore/cosign/v2/pkg/signature"
	"gopkg.in/yaml.v2"
)

// Tag represents a repository tag with metadata
type Tag struct {
	Name        string
	LastUpdated time.Time
	Digest      string
	Content     []byte // Raw content of the artifact
	ContentType string // Content type of the artifact
}

// OCIRepositoryInfo represents information about a repository
type OCIRepositoryInfo struct {
	LastUpdated   time.Time
	Tags          []Tag // All available tags
	AppliedTags   []Tag // Tags that have been successfully applied
	DiscardedTags []Tag // Tags that were discarded (either already applied or don't match regex)
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
	lastRepoCheck   OCIRepositoryInfo
	currentRepoInfo OCIRepositoryInfo
	history         TaskHistory
)

// Rules represents the structure of the rules YAML file
type Rules struct {
	RepositoryURL string `yaml:"repository_url"`
	Only          string `yaml:"only"`
	matcher       *regexp.Regexp
}

// Compile compiles the regex pattern for matching tags
func (r *Rules) Compile() error {
	var err error
	r.matcher, err = regexp.Compile(r.Only)
	return err
}

// Matches checks if a tag matches the regex pattern
func (r *Rules) Matches(tag string) bool {
	if r.matcher == nil {
		if err := r.Compile(); err != nil {
			log.Printf("Error compiling regex: %v", err)
			return false
		}
	}
	return r.matcher.MatchString(tag)
}

func NewRules() *Rules {
	return &Rules{
		RepositoryURL: "",
		Only:          "",
		matcher:       nil,
	}
}

func (r *Rules) Print() {
	log.Println("")
	log.Println("--------------------------")
	log.Println("--- GitOps Agent Rules ---")
	log.Printf("Repository URL: %s\n", r.RepositoryURL)
	log.Printf("Only pattern: %s\n", r.Only)
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

	// Convert string tags to Tag structs with metadata
	newTags := make([]Tag, 0)
	for _, tagName := range tagsResponse.Tags {
		// Get tag metadata by making a HEAD request
		tagURL := fmt.Sprintf("http://%s/v2/%s/manifests/%s", registry, repository, tagName)
		resp, err := http.Head(tagURL)
		if err != nil {
			log.Printf("Warning: Error getting metadata for tag %s: %v", tagName, err)
			continue
		}
		digest := resp.Header.Get("Docker-Content-Digest")
		lastModified := resp.Header.Get("Last-Modified")
		lastUpdated, _ := time.Parse(time.RFC1123, lastModified)

		// Fetch artifact content
		content, contentType, err := fetchArtifactContent(registry, repository, tagName, digest)
		if err != nil {
			log.Printf("Warning: Error fetching content for tag %s: %v", tagName, err)
		}

		newTags = append(newTags, Tag{
			Name:        tagName,
			LastUpdated: lastUpdated,
			Digest:      digest,
			Content:     content,
			ContentType: contentType,
		})
	}

	// Process and filter tags
	eligibleTags := make([]Tag, 0)
	discardedTags := make([]Tag, 0)

	// First pass: identify eligible and discarded tags
	for _, newTag := range newTags {
		// Check if tag matches the regex pattern
		if !r.Matches(newTag.Name) {
			discardedTags = append(discardedTags, newTag)
			continue
		}

		// Check if tag was already applied
		alreadyApplied := false
		for _, appliedTag := range currentRepoInfo.AppliedTags {
			if appliedTag.Name == newTag.Name && appliedTag.Digest == newTag.Digest {
				alreadyApplied = true
				discardedTags = append(discardedTags, newTag)
				break
			}
		}

		if !alreadyApplied {
			eligibleTags = append(eligibleTags, newTag)
		}
	}

	// Sort eligible tags by version (assuming semantic versioning)
	sort.Slice(eligibleTags, func(i, j int) bool {
		// Extract versions from tag names (assuming format v1.2.3 or similar)
		versionI := extractVersion(eligibleTags[i].Name)
		versionJ := extractVersion(eligibleTags[j].Name)
		return compareVersions(versionI, versionJ) > 0 // Sort in descending order
	})

	// Select the latest eligible tag if available
	hasChanges := false
	if len(eligibleTags) > 0 {
		latestTag := eligibleTags[0] // Get the highest version
		// Check if this tag is different from our last applied tag
		if len(currentRepoInfo.AppliedTags) == 0 ||
			currentRepoInfo.AppliedTags[len(currentRepoInfo.AppliedTags)-1].Digest != latestTag.Digest {
			hasChanges = true
			// We'll add this tag to AppliedTags when we actually apply the changes
			log.Printf("Found new eligible tag to apply: %s", latestTag.Name)
		}
	}

	// Update current info with new state
	currentRepoInfo.Tags = newTags
	currentRepoInfo.LastUpdated = time.Now()
	currentRepoInfo.DiscardedTags = discardedTags

	// If we found changes and have eligible tags, update the applied tags
	if hasChanges && len(eligibleTags) > 0 {
		latestTag := eligibleTags[0]
		currentRepoInfo.AppliedTags = append(currentRepoInfo.AppliedTags, latestTag)
	}

	// Save the current state for next comparison
	lastRepoCheck = currentRepoInfo

	history.Add("CheckRepository", fmt.Sprintf("Checked repository %s for changes. Found changes: %v", r.RepositoryURL, hasChanges))
	return hasChanges, nil
}

// ChangeSet represents changes in the repository
type ChangeSet struct {
	NewTags     []string
	RemovedTags []string
	UpdatedTags []string
}

func getRepositoryChanges() ChangeSet {
	changes := ChangeSet{
		NewTags:     make([]string, 0),
		RemovedTags: make([]string, 0),
		UpdatedTags: make([]string, 0),
	}

	// Find new and updated tags
	for _, newTag := range currentRepoInfo.Tags {
		found := false
		for _, oldTag := range lastRepoCheck.Tags {
			if oldTag.Name == newTag.Name {
				found = true
				// Compare metadata to detect updates
				if oldTag.Digest != newTag.Digest {
					changes.UpdatedTags = append(changes.UpdatedTags, newTag.Name)
				}
				break
			}
		}
		if !found {
			changes.NewTags = append(changes.NewTags, newTag.Name)
		}
	}

	// Find removed tags
	for _, oldTag := range lastRepoCheck.Tags {
		found := false
		for _, newTag := range currentRepoInfo.Tags {
			if oldTag.Name == newTag.Name {
				found = true
				break
			}
		}
		if !found {
			changes.RemovedTags = append(changes.RemovedTags, oldTag.Name)
		}
	}

	return changes
}

func fetchArtifactContent(registry, repository, tag, digest string) ([]byte, string, error) {
	// First try to get the manifest
	manifestURL := fmt.Sprintf("http://%s/v2/%s/manifests/%s", registry, repository, tag)
	req, err := http.NewRequest("GET", manifestURL, nil)
	if err != nil {
		return nil, "", fmt.Errorf("error creating request: %v", err)
	}

	// Add accept headers for different manifest types
	req.Header.Add("Accept", "application/vnd.oci.image.manifest.v1+json")
	req.Header.Add("Accept", "application/vnd.docker.distribution.manifest.v2+json")

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return nil, "", fmt.Errorf("error fetching manifest: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	contentType := resp.Header.Get("Content-Type")
	content, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, "", fmt.Errorf("error reading content: %v", err)
	}

	return content, contentType, nil
}

func displayArtifactContent(tag Tag) string {
	if len(tag.Content) == 0 {
		return "No content available"
	}

	// Try to pretty print JSON content
	if tag.ContentType == "application/json" ||
		tag.ContentType == "application/vnd.oci.image.manifest.v1+json" ||
		tag.ContentType == "application/vnd.docker.distribution.manifest.v2+json" {
		var prettyJSON bytes.Buffer
		if err := json.Indent(&prettyJSON, tag.Content, "", "  "); err == nil {
			return prettyJSON.String()
		}
	}

	// Return raw content if not JSON or if JSON parsing fails
	return string(tag.Content)
}

// Version represents a semantic version
type Version struct {
	Major int
	Minor int
	Patch int
}

// extractVersion extracts version numbers from a tag name
// Supports formats like "v1.2.3", "1.2.3", "v1.2", "1.2"
func extractVersion(tagName string) Version {
	// Remove 'v' prefix if present
	version := strings.TrimPrefix(tagName, "v")

	// Split version string into parts
	parts := strings.Split(version, ".")

	// Initialize version numbers
	var major, minor, patch int

	// Parse major version
	if len(parts) > 0 {
		major, _ = strconv.Atoi(parts[0])
	}

	// Parse minor version
	if len(parts) > 1 {
		minor, _ = strconv.Atoi(parts[1])
	}

	// Parse patch version
	if len(parts) > 2 {
		patch, _ = strconv.Atoi(parts[2])
	}

	return Version{Major: major, Minor: minor, Patch: patch}
}

// compareVersions compares two versions
// Returns:
//
//	1 if v1 > v2
//	0 if v1 == v2
//	-1 if v1 < v2
func compareVersions(v1, v2 Version) int {
	// Compare major version
	if v1.Major != v2.Major {
		if v1.Major > v2.Major {
			return 1
		}
		return -1
	}

	// Compare minor version
	if v1.Minor != v2.Minor {
		if v1.Minor > v2.Minor {
			return 1
		}
		return -1
	}

	// Compare patch version
	if v1.Patch != v2.Patch {
		if v1.Patch > v2.Patch {
			return 1
		}
		return -1
	}

	// Versions are equal
	return 0
}

func parseRulesFile() {

	if rules != nil {
		log.Println("Deleting existing rules...")
		rules = nil
	}

	rules = NewRules()

	log.Printf("Reading rules file: %s", rules_file)
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
	changes := getRepositoryChanges()

	if len(changes.NewTags) > 0 {
		log.Printf("New tags: %v", changes.NewTags)
	}
	if len(changes.UpdatedTags) > 0 {
		log.Printf("Updated tags: %v", changes.UpdatedTags)
	}
	if len(changes.RemovedTags) > 0 {
		log.Printf("Removed tags: %v", changes.RemovedTags)
	}

	var eligibleTags []Tag
	for _, tag := range currentRepoInfo.Tags {
		if wasAlreadyApplied(tag) {
			continue
		}
		if !rules.Matches(tag.Name) {
			log.Printf("Skipping tag %s: doesn't match allowed pattern", tag.Name)
			currentRepoInfo.DiscardedTags = append(currentRepoInfo.DiscardedTags, tag)
			continue
		}
		eligibleTags = append(eligibleTags, tag)
	}

	sort.Slice(eligibleTags, func(i, j int) bool {
		return compareVersions(extractVersion(eligibleTags[i].Name), extractVersion(eligibleTags[j].Name)) > 0
	})

	if len(eligibleTags) == 0 {
		log.Println("No eligible tags found to apply")
		return
	}

	pubKeyPath := os.Getenv("COSIGN_PUBLIC_KEY")
	if pubKeyPath == "" {
		log.Println("COSIGN_PUBLIC_KEY not set, skipping verification")
		return
	}

	pubKey, err := signature.LoadPublicKey(pubKeyPath)
	if err != nil {
		log.Printf("Failed to load Cosign public key: %v", err)
		return
	}

	registry, repo := strings.SplitN(rules.RepositoryURL, "/", 2)

	for _, tag := range eligibleTags {
		ref := fmt.Sprintf("%s:%s", currentRepoInfo.Repository, tag.Name)
		log.Printf("🔍 Verifying signature for artifact: %s (digest: %s)", ref, tag.Digest)

		sigs, _, err := cosign.VerifyImageSignatures(context.Background(), ref, &cosign.CheckOpts{
			Claims:             true,
			Tlog:               false,
			Offline:            false,
			SigVerifier:        pubKey,
			RegistryClientOpts: []remote.Option{registry.WithInsecure(true)},
		})

		if err != nil {
			log.Printf("❌ Invalid signature for tag %s: %v", tag.Name, err)
			currentRepoInfo.DiscardedTags = append(currentRepoInfo.DiscardedTags, tag)

			if registry != "" && tag.Digest != "" {
				deleteURL := fmt.Sprintf("http://%s/v2/%s/manifests/%s", registry, repository, tag.Name)
				log.Printf("🧹 Attempting to delete invalid artifact via DELETE: %s", deleteURL)

				resp, delErr := http.NewRequest("DELETE", deleteURL, nil)
				if delErr != nil {
					log.Printf("⚠️ Failed to create delete request: %v", delErr)
					continue
				}
				client := &http.Client{}
				res, doErr := client.Do(resp)
				if doErr != nil {
					log.Printf("⚠️ Failed to delete invalid artifact: %v", doErr)
				} else {
					log.Printf("🗑️ Delete response: %s", res.Status)
					res.Body.Close()
				}
			}
			continue
		}

		log.Printf("✅ Signature verified for tag %s", tag.Name)
		log.Printf("📦 Applying artifact content:\n%s", displayArtifactContent(tag))

		currentRepoInfo.AppliedTags = append(currentRepoInfo.AppliedTags, tag)
		history.Add("ApplyTag", fmt.Sprintf("Applied tag %s (digest %s)", tag.Name, tag.Digest))
		return
	}

	log.Println("❗ No signed and eligible tags were verified successfully.")
}

// Helper function to check if a tag is in a slice of tags
func containsTag(tags []Tag, tag Tag) bool {
	for _, t := range tags {
		if t.Name == tag.Name && t.Digest == tag.Digest {
			return true
		}
	}
	return false
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
				log.Printf("Repository check result: %v", changed)
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
}

func main() {

	// Set up signal handling
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	// Create channel for rules file watcher
	ruleWatcherStop := make(chan bool)

	// Start rules file watcher in a goroutine
	go watchRulesFile(ruleWatcherStop)

	// Start HTTP server in a goroutine
	serverChan := make(chan error)
	go func() {
		port := os.Getenv("PORT")
		if port == "" {
			port = "8080"
		}
		log.Printf("Starting server on port %s", port)
		startHTTPServer(port)
	}()

	// Mark service as ready
	ready = true
	log.Println("GitOps agent is ready")

	// Wait for signals
	select {
	case err := <-serverChan:
		log.Printf("Server error: %v", err)
	case sig := <-sigChan:
		log.Printf("Received signal: %v", sig)
	}

	// Cleanup
	log.Println("Shutting down...")
	ruleWatcherStop <- true
	close(ruleWatcherStop)

	// Final status report
	log.Printf("Final status - Applied tags: %d, Discarded tags: %d",
		len(currentRepoInfo.AppliedTags),
		len(currentRepoInfo.DiscardedTags))

	log.Println("Shutdown complete")
}
