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
	"strings"
	"sync"
	"syscall"
	"time"

	"gopkg.in/yaml.v3"
)

// Tag represents a repository tag with metadata
type Tag struct {
	Name        string
	LastUpdated time.Time
	Digest      string
	Sig         []byte
	Content     []byte
	ContentType string
}

func (t Tag) Print() {
	log.Printf("Tag: %s, Digest: %s, Last Updated: %s, SIG %s, Content Type: %s",
		t.Name, t.Digest, t.LastUpdated.Format(time.RFC3339), t.Sig, t.ContentType)
	if len(t.Content) > 0 {
		log.Printf("Content: %s", string(t.Content))
	} else {
		log.Println("Content: <empty>")
	}
}

// OCIRepositoryInfo represents information about a repository
type OCIRepositoryInfo struct {
	LastUpdated time.Time
	Tags        []Tag             // All available tags
	Applied     map[string]string // Map of tagName -> digest for applied tags
}

func NewOCIRepositoryInfo() OCIRepositoryInfo {
	return OCIRepositoryInfo{
		Tags:    make([]Tag, 0),
		Applied: make(map[string]string),
	}
}

// TaskHistory keeps track of all operations
type TaskHistory struct {
	mu      sync.RWMutex
	entries []HistoryEntry
}

type HistoryEntry struct {
	Timestamp time.Time
	Operation string
	Details   string
}

func (h *TaskHistory) AddEntry(operation, details string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.entries = append(h.entries, HistoryEntry{
		Timestamp: time.Now(),
		Operation: operation,
		Details:   details,
	})
}

type Rules struct {
	RepositoryURL string `yaml:"repository_url"`
	Only          string `yaml:"only"`
	matcher       *regexp.Regexp
}

func (r *Rules) Matches(tag string) bool {
	if r.matcher == nil {
		var err error
		r.matcher, err = regexp.Compile(r.Only)
		if err != nil {
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

var (
	rules      *Rules
	rules_file string
	ready      bool
	repoState  OCIRepositoryInfo
	stopChan   chan struct{}
	history    TaskHistory
)

func (r *Rules) checkRepository() error {
	parts := regexp.MustCompile(`^([^/]+)/(.+)$`).FindStringSubmatch(r.RepositoryURL)
	if len(parts) != 3 {
		return fmt.Errorf("invalid repository URL format: %s", r.RepositoryURL)
	}
	registry, repository := parts[1], parts[2]

	tags, err := fetchTags(registry, repository)
	if err != nil {
		return fmt.Errorf("error fetching tags: %w", err)
	}

	for _, tag := range tags {
		if strings.HasSuffix(tag.Name, ".sig") || !r.Matches(tag.Name) {
			continue
		}

		if digest, exists := repoState.Applied[tag.Name]; exists && digest == tag.Digest {
			continue
		}

		log.Printf("Found new eligible tag: %s with digest %s", tag.Name, tag.Digest)
		if err := processTag(tag); err != nil {
			log.Printf("Error processing tag %s: %v", tag.Name, err)
			continue
		}
	}

	repoState.Tags = tags
	repoState.LastUpdated = time.Now()
	return nil
}

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

func fetchTagMetadata(registry, repository, tagName string) (Tag, error) {
	tagURL := fmt.Sprintf("http://%s/v2/%s/manifests/%s", registry, repository, tagName)
	req, err := http.NewRequest("GET", tagURL, nil)
	if err != nil {
		return Tag{}, err
	}
	req.Header.Add("Accept", "application/vnd.oci.image.manifest.v1+json")
	req.Header.Add("Accept", "application/vnd.docker.distribution.manifest.v2+json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return Tag{}, err
	}
	defer resp.Body.Close()

	digest := resp.Header.Get("Docker-Content-Digest")
	var manifestResponse struct {
		Layers []struct {
			Digest string `json:"digest"`
		} `json:"layers"`
		Config struct {
			Digest string `json:"digest"`
		} `json:"config"`
	}

	if err := json.NewDecoder(resp.Body).Decode(&manifestResponse); err == nil {
		if manifestResponse.Config.Digest != "" {
			digest = manifestResponse.Config.Digest
		} else if len(manifestResponse.Layers) > 0 {
			digest = manifestResponse.Layers[0].Digest
		}
	} else {
		log.Printf("Warning: Error decoding manifest response for tag %s: %v", tagName, err)
	}

	lastModified := resp.Header.Get("Last-Modified")
	lastUpdated, _ := time.Parse(time.RFC1123, lastModified)

	sigContent, content, contentType, err := fetchArtifactContent(registry, repository, tagName, digest)
	if err != nil {
		log.Printf("Warning: Error fetching content for tag %s: %v", tagName, err)
	}

	return Tag{
		Name:        tagName,
		LastUpdated: lastUpdated,
		Digest:      digest,
		Sig:         sigContent,
		Content:     content,
		ContentType: contentType,
	}, nil
}

func fetchArtifactContent(registry, repository, tag, digest string) ([]byte, []byte, string, error) {

	digest = strings.Replace(digest, ":", "-", 1)
	sigManifestURL := fmt.Sprintf("http://%s/v2/%s/manifests/%s.sig", registry, repository, digest)
	log.Printf("Fetching signature manifest from %s", sigManifestURL)
	req, err := http.NewRequest("GET", sigManifestURL, nil)
	if err != nil {
		return nil, nil, "", fmt.Errorf("error creating request for signature verification: %v", err)
	}
	sigBody, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, nil, "", fmt.Errorf("error fetching signature manifest: %v", err)
	}
	defer sigBody.Body.Close()
	log.Printf("Signature manifest response status from %s: %s", sigManifestURL, sigBody.Status)
	if sigBody.StatusCode != http.StatusOK {
		return nil, nil, "", fmt.Errorf("unexpected status code for signature manifest: %d", sigBody.StatusCode)
	}
	log.Printf("Signature manifest for tag %s fetched successfully", tag)
	sigContent, err := io.ReadAll(sigBody.Body)
	if err != nil {
		return nil, nil, "", fmt.Errorf("error reading signature manifest content: %v", err)
	}
	log.Printf("Contents: %s. %s", sigBody.Header, string(sigContent))

	manifestURL := fmt.Sprintf("http://%s/v2/%s/manifests/%s", registry, repository, tag)
	req, err = http.NewRequest("GET", manifestURL, nil)
	if err != nil {
		return nil, nil, "", fmt.Errorf("error creating request: %v", err)
	}

	req.Header.Add("Accept", "application/vnd.oci.image.manifest.v1+json")
	req.Header.Add("Accept", "application/vnd.docker.distribution.manifest.v2+json")

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return nil, nil, "", fmt.Errorf("error fetching manifest: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, nil, "", fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	contentType := resp.Header.Get("Content-Type")
	content, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, nil, "", fmt.Errorf("error reading content: %v", err)
	}

	return sigContent, content, contentType, nil
}

func verifyTag(tag Tag) error {
	log.Printf("Verifying tag: %s (digest: %s)", tag.Name, tag.Digest)

	// Here you would implement any verification logic needed for the tag.
	// For example, checking signatures or validating content.
	// This is a placeholder for demonstration purposes.

	tag.Print()

	if tag.ContentType != "application/vnd.oci.image.manifest.v1+json" {
		return fmt.Errorf("invalid content type for tag %s: %s", tag.Name, tag.ContentType)
	}

	log.Printf("Tag %s verified successfully", tag.Name)
	return nil
}

func processTag(tag Tag) error {
	ready = false
	log.Printf("Processing tag: %s (digest: %s)", tag.Name, tag.Digest)
	verifyTag(tag)
	repoState.Applied[tag.Name] = tag.Digest
	history.AddEntry("ApplyTag", fmt.Sprintf("Applied tag %s (digest %s)", tag.Name, tag.Digest))
	ready = true
	log.Printf("Tag %s processed successfully", tag.Name)
	return nil
}

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

func parseRulesFile() {
	if rules != nil {
		rules = nil
	}

	rules = NewRules()
	log.Printf("Reading rules file: %s", rules_file)

	data, err := os.ReadFile(rules_file)
	if err != nil {
		log.Fatalf("Error reading rules file: %v", err)
	}

	if err := yaml.Unmarshal(data, rules); err != nil {
		log.Fatalf("Error parsing YAML: %v", err)
	}
}

func statusHandler(w http.ResponseWriter, r *http.Request) {
	if rules != nil && ready {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("GitOps Agent is ready"))
	} else {
		w.WriteHeader(http.StatusServiceUnavailable)
		w.Write([]byte("GitOps Agent is not ready"))
	}
}

func alive(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("GitOps Agent is alive"))
}

func init() {
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
	repoState = NewOCIRepositoryInfo()
	stopChan = make(chan struct{})

	sigChan := make(chan os.Signal, 1)
	signal.Notify(sigChan, syscall.SIGINT, syscall.SIGTERM)

	startRepositoryWatcher()
	log.Println("Repository watcher started")

	mux := http.NewServeMux()
	mux.HandleFunc("/", alive)
	mux.HandleFunc("/status", statusHandler)

	server := &http.Server{
		Addr:    ":8080",
		Handler: mux,
	}

	go func() {
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("Server error: %v", err)
		}
	}()

	log.Printf("HTTP server listening on :8080")

	ready = true

	<-sigChan
	log.Println("Shutting down...")

	close(stopChan)
	log.Printf("Final status - Applied tags: %d", len(repoState.Applied))
	log.Println("Shutdown complete")
}
