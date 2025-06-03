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
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/sigstore/cosign/v2/pkg/cosign"
	"github.com/sigstore/cosign/v2/pkg/oci/remote"
	"github.com/sigstore/cosign/v2/pkg/signature"
	cosignsig "github.com/sigstore/sigstore/pkg/signature"
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
	Tags          []Tag             // All available tags
	Applied       map[string]string // Map of tagName -> digest for applied tags
	DiscardedTags []Tag             // Tags that failed verification
}

// NewOCIRepositoryInfo creates a new repository info instance
func NewOCIRepositoryInfo() OCIRepositoryInfo {
	return OCIRepositoryInfo{
		Tags:    make([]Tag, 0),
		Applied: make(map[string]string),
	}
}

// HistoryEntry represents a single operation in the agent's history
type HistoryEntry struct {
	Timestamp time.Time
	Operation string
	Details   string
}

// TaskHistory keeps track of all operations
type TaskHistory struct {
	mu      sync.RWMutex
	entries []HistoryEntry
}

var (
	rules      *Rules
	rules_file string
	repoState  OCIRepositoryInfo
	stopChan   chan struct{}
	history    TaskHistory
)

// AddHistoryEntry adds a new entry to the history
func (h *TaskHistory) AddEntry(operation, details string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.entries = append(h.entries, HistoryEntry{
		Timestamp: time.Now(),
		Operation: operation,
		Details:   details,
	})
}

// GetHistory returns all history entries
func (h *TaskHistory) GetHistory() []HistoryEntry {
	h.mu.RLock()
	defer h.mu.RUnlock()
	result := make([]HistoryEntry, len(h.entries))
	copy(result, h.entries)
	return result
}

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

// checkRepository checks for new tags and processes eligible ones
func (r *Rules) checkRepository() error {
	parts := regexp.MustCompile(`^([^/]+)/(.+)$`).FindStringSubmatch(r.RepositoryURL)
	if len(parts) != 3 {
		return fmt.Errorf("invalid repository URL format: %s", r.RepositoryURL)
	}
	registry, repository := parts[1], parts[2]

	// Get list of tags
	tags, err := fetchTags(registry, repository)
	if err != nil {
		return fmt.Errorf("error fetching tags: %w", err)
	}

	// Process eligible tags
	for _, tag := range tags {
		// Skip signature files and non-matching tags
		if strings.HasSuffix(tag.Name, ".sig") || !r.Matches(tag.Name) {
			continue
		}

		// Check if already processed
		if digest, exists := repoState.Applied[tag.Name]; exists && digest == tag.Digest {
			continue
		}

		// Process new eligible tag
		log.Printf("Found new eligible tag: %s with digest %s", tag.Name, tag.Digest)
		if err := processTag(tag); err != nil {
			log.Printf("Error processing tag %s: %v", tag.Name, err)
			continue
		}
	}

	// Update state
	repoState.Tags = tags
	repoState.LastUpdated = time.Now()
	return nil
}

// fetchTags gets all tags from the registry
func fetchTags(registry, repository string) ([]Tag, error) {
	url := fmt.Sprintf("http://%s/v2/%s/tags/list", registry, repository)
	resp, err := http.Get(url)
	if err != nil {
		return nil, fmt.Errorf("error checking repository: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	var tagsResponse struct {
		Name string   `json:"name"`
		Tags []string `json:"tags"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tagsResponse); err != nil {
		return nil, fmt.Errorf("error parsing JSON: %w", err)
	}

	tags := make([]Tag, 0, len(tagsResponse.Tags))
	for _, tagName := range tagsResponse.Tags {
		tag, err := fetchTagMetadata(registry, repository, tagName)
		if err != nil {
			log.Printf("Warning: Error getting metadata for tag %s: %v", tagName, err)
			continue
		}
		tags = append(tags, tag)
	}

	return tags, nil
}

// fetchTagMetadata gets metadata for a single tag
func fetchTagMetadata(registry, repository, tagName string) (Tag, error) {
	tagURL := fmt.Sprintf("http://%s/v2/%s/manifests/%s", registry, repository, tagName)
	resp, err := http.Head(tagURL)
	if err != nil {
		return Tag{}, err
	}
	defer resp.Body.Close()

	digest := resp.Header.Get("Docker-Content-Digest")
	lastModified := resp.Header.Get("Last-Modified")
	lastUpdated, _ := time.Parse(time.RFC1123, lastModified)

	content, contentType, err := fetchArtifactContent(registry, repository, tagName, digest)
	if err != nil {
		log.Printf("Warning: Error fetching content for tag %s: %v", tagName, err)
	}

	return Tag{
		Name:        tagName,
		LastUpdated: lastUpdated,
		Digest:      digest,
		Content:     content,
		ContentType: contentType,
	}, nil
}

// processTag verifies and applies a single tag
func processTag(tag Tag) error {
	pubKeyPath := os.Getenv("COSIGN_PUBLIC_KEY")
	if pubKeyPath == "" {
		return fmt.Errorf("COSIGN_PUBLIC_KEY not set")
	}

	log.Printf("Processing tag: %s (digest: %s) using public key %s", tag.Name, tag.Digest, pubKeyPath)
	ctx := context.Background()

	pubKey, err := signature.LoadPublicKey(ctx, pubKeyPath)
	if err != nil {
		return fmt.Errorf("failed to load public key: %w", err)
	}

	parts := strings.SplitN(rules.RepositoryURL, "/", 2)
	if len(parts) != 2 {
		return fmt.Errorf("invalid repository URL")
	}
	registry, repository := parts[0], parts[1]

	verified, err := verifyAndProcessTag(ctx, tag, pubKey, registry, repository)
	if err != nil {
		return fmt.Errorf("verification failed: %w", err)
	}

	if verified {
		log.Printf("✅ Successfully verified and applied tag %s", tag.Name)
		repoState.Applied[tag.Name] = tag.Digest
		history.AddEntry("ApplyTag", fmt.Sprintf("Applied tag %s (digest %s)", tag.Name, tag.Digest))
	}

	return nil
}

// startRepositoryWatcher starts a goroutine to periodically check the repository
func startRepositoryWatcher() {
	ticker := time.NewTicker(1 * time.Minute)
	go func() {
		for {
			select {
			case <-ticker.C:
				if err := rules.checkRepository(); err != nil {
					log.Printf("Error checking repository: %v", err)
				}
			case <-stopChan:
				ticker.Stop()
				return
			}
		}
	}()
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

	for _, tag := range repoState.Tags {
		if digest, exists := repoState.Applied[tag.Name]; !exists || digest != tag.Digest {
			changes.NewTags = append(changes.NewTags, tag.Name)
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
				if err := rules.checkRepository(); err != nil {
					log.Printf("Error checking repository: %v", err)
				} else {
					log.Println("Repository checked successfully")
				}
			}
		}
	}
}

// Helper function to check if a tag was already applied
func wasAlreadyApplied(tag Tag) bool {
	if digest, exists := repoState.Applied[tag.Name]; exists && digest == tag.Digest {
		return true
	}
	return false
}

func getChanges() ChangeSet {
	changes := ChangeSet{
		NewTags:     make([]string, 0),
		RemovedTags: make([]string, 0),
		UpdatedTags: make([]string, 0),
	}
	return changes
}

func processRepositoryChanges() {
	changes := getRepositoryChanges()
	log.Printf("Repository changes identified %d new, %d updated and %d removed", len(changes.NewTags), len(changes.UpdatedTags), len(changes.RemovedTags))

	if len(changes.NewTags) > 0 {
		log.Printf("New tags: %v", changes.NewTags)
	}
	if len(changes.UpdatedTags) > 0 {
		log.Printf("Updated tags: %v", changes.UpdatedTags)
	}
	if len(changes.RemovedTags) > 0 {
		log.Printf("Removed tags: %v", changes.RemovedTags)
	}

	var newTags []Tag
	for _, tag := range repoState.Tags {
		// Skip signature files
		if strings.HasSuffix(tag.Name, ".sig") {
			log.Printf("Skipping signature file: %s", tag.Name)
			continue
		}

		// Check if tag matches the regex pattern
		if !rules.Matches(tag.Name) {
			log.Printf("Tag %s does not match pattern %s, skipping", tag.Name, rules.Only)
			continue
		}

		// Check if already processed
		if wasAlreadyApplied(tag) {
			continue
		}

		newTags = append(newTags, tag)
	}

	if len(newTags) == 0 {
		log.Printf("No new tags to process")
		return
	}

	log.Printf("Found %d new tag(s) to process", len(newTags))
	processNewTags(newTags)
}

// Helper function to get a list of tag names
func getCurrentTagNames(tags []Tag) []string {
	var names []string
	for _, tag := range tags {
		names = append(names, tag.Name)
	}
	return names
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

// verifyAndProcessTag verifies the signature of an OCI artifact tag using Cosign.
func verifyAndProcessTag(ctx context.Context, tag Tag, pubKey cosignsig.Verifier, registry, repository string) (bool, error) {
	log.Printf("🔍 Verifying tag: %s (digest: %s)", tag.Name, tag.Digest)

	imageRef := fmt.Sprintf("%s/%s:%s", registry, repository, tag.Name)
	ref, err := name.ParseReference(imageRef)
	if err != nil {
		return false, fmt.Errorf("failed to parse image reference %q: %w", imageRef, err)
	}

	checkOpts := &cosign.CheckOpts{
		RegistryClientOpts: []remote.Option{
			remote.WithTargetRepository(ref.Context()),
		},
		SigVerifier: pubKey,
		Offline:     false,
	}

	signatures, _, err := cosign.VerifyImageSignatures(ctx, ref, checkOpts)
	if err != nil || len(signatures) == 0 {
		log.Printf("❌ Signature verification failed for tag %q: %v", tag.Name, err)

		if tag.Digest != "" {
			deleteURL := fmt.Sprintf("http://%s/v2/%s/manifests/%s", registry, repository, tag.Digest)
			log.Printf("🧹 Deleting invalid artifact: %s", deleteURL)

			req, reqErr := http.NewRequest(http.MethodDelete, deleteURL, nil)
			if reqErr != nil {
				log.Printf("⚠️ Failed to create DELETE request: %v", reqErr)
				return false, fmt.Errorf("failed to create delete request: %w", reqErr)
			}

			resp, doErr := http.DefaultClient.Do(req)
			if doErr != nil {
				log.Printf("⚠️ Failed to delete artifact: %v", doErr)
			} else {
				defer resp.Body.Close()
				log.Printf("🗑️ DELETE response: %s", resp.Status)
			}
		}

		return false, fmt.Errorf("signature verification failed for tag %q", tag.Name)
	}

	log.Printf("✅ Signature verified for tag %q", tag.Name)
	return true, nil
}

// processNewTags handles verification and processing of new tags
func processNewTags(newTags []Tag) {
	pubKeyPath := os.Getenv("COSIGN_PUBLIC_KEY")
	if pubKeyPath == "" {
		log.Println("COSIGN_PUBLIC_KEY not set, skipping verification")
		return
	}

	ctx := context.Background()
	pubKey, err := signature.LoadPublicKey(ctx, pubKeyPath)
	if err != nil {
		log.Printf("Failed to load Cosign public key: %v", err)
		return
	}

	urlParts := strings.SplitN(rules.RepositoryURL, "/", 2)
	if len(urlParts) != 2 {
		log.Printf("Invalid repository URL format: %s", rules.RepositoryURL)
		return
	}
	registry, repository := urlParts[0], urlParts[1]

	for _, tag := range newTags {
		verified, err := verifyAndProcessTag(ctx, tag, pubKey, registry, repository)
		if err != nil {
			log.Printf("❌ Verification failed for tag %s: %v", tag.Name, err)
			repoState.DiscardedTags = append(repoState.DiscardedTags, tag)
			continue
		}

		if verified {
			log.Printf("✅ Successfully verified tag %s", tag.Name)
			repoState.Applied[tag.Name] = tag.Digest
			log.Printf("Successfully applied tag %s (digest %s)", tag.Name, tag.Digest)
		}
	}
}

// handleEligibleTag processes a single eligible tag immediately
func handleEligibleTag(tag Tag) {
	pubKeyPath := os.Getenv("COSIGN_PUBLIC_KEY")
	if pubKeyPath == "" {
		log.Println("COSIGN_PUBLIC_KEY not set, skipping verification")
		return
	}

	ctx := context.Background()
	pubKey, err := signature.LoadPublicKey(ctx, pubKeyPath)
	if err != nil {
		log.Printf("Failed to load Cosign public key: %v", err)
		return
	}

	urlParts := strings.SplitN(rules.RepositoryURL, "/", 2)
	if len(urlParts) != 2 {
		log.Printf("Invalid repository URL format: %s", rules.RepositoryURL)
		return
	}
	registry, repository := urlParts[0], urlParts[1]

	verified, err := verifyAndProcessTag(ctx, tag, pubKey, registry, repository)
	if err != nil {
		log.Printf("❌ Verification failed for tag %s: %v", tag.Name, err)
		repoState.DiscardedTags = append(repoState.DiscardedTags, tag)
		return
	}

	if verified {
		log.Printf("✅ Successfully verified tag %s", tag.Name)
		repoState.Applied[tag.Name] = tag.Digest
		history.AddEntry("ApplyTag", fmt.Sprintf("Applied tag %s (digest %s)", tag.Name, tag.Digest))
	}
}

// statusHandler returns the agent's readiness status
func statusHandler(w http.ResponseWriter, r *http.Request) {
	if rules != nil && repoState.LastUpdated.After(time.Time{}) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("GitOps Agent is ready"))
	} else {
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte("GitOps Agent is not ready"))
	}
}

// alive is a simple liveness check endpoint
func alive(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("GitOps Agent is alive"))
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
	history.AddEntry("Startup", "Agent initialized with rules file: "+rules_file)
}

func main() {
	// Initialize repository state
	repoState = NewOCIRepositoryInfo()
	stopChan = make(chan struct{})

	// Set up signal handling
	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	// Start repository watcher
	startRepositoryWatcher()
	log.Println("GitOps agent is ready")

	// Start HTTP server with status endpoints
	mux := http.NewServeMux()
	mux.HandleFunc("/", alive)               // Root endpoint for liveness
	mux.HandleFunc("/status", statusHandler) // Status endpoint for readiness

	server := &http.Server{
		Addr:    ":8080",
		Handler: mux,
	}

	go func() {
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("Server error: %v", err)
		}
	}()

	// Wait for shutdown signal
	<-sigChan
	log.Println("Shutting down...")

	// Clean shutdown
	close(stopChan)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil {
		log.Printf("Error during shutdown: %v", err)
	}

	log.Printf("Final status - Applied tags: %d", len(repoState.Applied))
	log.Println("Shutdown complete")
}
