package main

import (
	"encoding/json"
	"context"
	"crypto"
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

	// "github.com/sigstore/sigstore/pkg/cryptoutils"
	// "github.com/sigstore/sigstore/pkg/signature"
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/sigstore/cosign/v2/pkg/cosign"
	"github.com/sigstore/cosign/v2/pkg/oci/remote"
	"github.com/sigstore/sigstore/pkg/cryptoutils"
	"github.com/sigstore/sigstore/pkg/signature"
	"gopkg.in/yaml.v3"
)

// Tag represents a repository tag with metadata
type Tag struct {
	Name        string
	LastUpdated time.Time
	Digest      string
	Signature   string
	Content     []byte
	ContentType string
}

func (t Tag) Print() {
	log.Printf("Tag: %s, Digest: %s, Last Updated: %s, Content Type: %s",
		t.Name, t.Digest, t.LastUpdated.Format(time.RFC3339), t.ContentType)
	log.Printf("Signature (hex): %x", t.Signature)
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

		if strings.HasSuffix(tagName, ".sig") || !rules.Matches(tagName) {
			log.Printf("Skipping tag %s because it doesn't match rules or is a signature", tagName)
			continue
		}

		log.Printf("Found tag: %s", tagName)
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

	log.Printf("Tag %s has digest %s", tagName, digest)
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
		Signature:   sigContent,
		Content:     content,
		ContentType: contentType,
	}, nil
}

func fetchArtifactContent(registry, repository, tag, digest string) (string, []byte, string, error) {
	digest = strings.Replace(digest, ":", "-", 1)
	sigManifestURL := fmt.Sprintf("http://%s/v2/%s/manifests/%s.sig", registry, repository, digest)
	log.Printf("Fetching signature manifest from %s", sigManifestURL)
	req, err := http.NewRequest("GET", sigManifestURL, nil)
	if err != nil {
		return "", nil, "", fmt.Errorf("error creating request for signature verification: %v", err)
	}

	// Add manifest media type headers for signature
	req.Header.Add("Accept", "application/vnd.oci.image.manifest.v1+json")
	req.Header.Add("Accept", "application/vnd.docker.distribution.manifest.v2+json")

	sigBody, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", nil, "", fmt.Errorf("error fetching signature manifest: %v", err)
	}
	defer sigBody.Body.Close()

	if sigBody.StatusCode != http.StatusOK {
		return "", nil, "", fmt.Errorf("unexpected status code for signature manifest: %d", sigBody.StatusCode)
	}

	// Parse the signature manifest to get annotations
	var sigManifest struct {
		Layers []struct {
			Annotations map[string]string `json:"annotations"`
		} `json:"layers"`
	}

	if err := json.NewDecoder(sigBody.Body).Decode(&sigManifest); err != nil {
		return "", nil, "", fmt.Errorf("error parsing signature manifest: %v", err)
	}

	// Extract signature content from first layer's annotations
	var sigContent string
	if len(sigManifest.Layers) > 0 && len(sigManifest.Layers[0].Annotations) > 0 {
		annotations := sigManifest.Layers[0].Annotations
		if sig, ok := annotations["dev.cosignproject.cosign/signature"]; ok {
			log.Printf("Cosign signature found for %s in annotations and loaded correctly!", tag)
			sigContent = sig
		} else {
			log.Printf("No cosign signature found in annotations: %+v", annotations)
			return "", nil, "", fmt.Errorf("no signature found in manifest annotations")
		}
	} else {
		return "", nil, "", fmt.Errorf("no layers or annotations found in signature manifest")
	}

	// Now get the main artifact content
	manifestURL := fmt.Sprintf("http://%s/v2/%s/manifests/%s", registry, repository, tag)
	req, err = http.NewRequest("GET", manifestURL, nil)
	if err != nil {
		return "", nil, "", fmt.Errorf("error creating request: %v", err)
	}

	req.Header.Add("Accept", "application/vnd.oci.image.manifest.v1+json")
	req.Header.Add("Accept", "application/vnd.docker.distribution.manifest.v2+json")

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return "", nil, "", fmt.Errorf("error fetching manifest: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return "", nil, "", fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	contentType := resp.Header.Get("Content-Type")
	content, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", nil, "", fmt.Errorf("error reading content: %v", err)
	}

	return sigContent, content, contentType, nil
}

func verifyTag(tag Tag) error {
	log.Printf("Verifying tag: %s (digest: %s)", tag.Name, tag.Digest)

	ctx := context.Background()

	imageRef := fmt.Sprintf("%s@%s", rules.RepositoryURL, tag.Digest)
	log.Printf("Image reference to verify: %s", imageRef)
	// Parse the image reference
	ref, err := name.ParseReference(imageRef, name.WithDefaultRegistry(""), name.Insecure)
	if err != nil {
		return fmt.Errorf("invalid image reference: %w", err)
	}

	pubKeyBytes, err := os.ReadFile(os.Getenv("COSIGN_PUBLIC_KEY"))
	if err != nil {
		return fmt.Errorf("failed to read public key file: %w", err)
	}

	// Parse the PEM-encoded public key
	pubKey, err := cryptoutils.UnmarshalPEMToPublicKey(pubKeyBytes)
	if err != nil {
		return fmt.Errorf("failed to parse public key: %w", err)
	}

	// Create a verifier using the public key
	verifier, err := signature.LoadVerifier(pubKey, crypto.SHA256)
	if err != nil {
		return fmt.Errorf("failed to create verifier: %w", err)
	}

	// Build verification options
	checkOpts := &cosign.CheckOpts{
		SigVerifier: verifier,
		RegistryClientOpts: []remote.Option{
			remote.WithRemoteOptions(), // handles basic auth, plain http if needed
		},
		IgnoreTlog: true, // ignore transparency log
		Offline: true, // no Rekor, no transparency log
	}

	// Perform verification
	sigs, _, err := cosign.VerifyImageSignatures(ctx, ref, checkOpts)
	if err != nil {
		return fmt.Errorf("signature verification failed: %w", err)
	}

	if len(sigs) == 0 {
		return fmt.Errorf("no signatures found for %s", imageRef)
	}

	log.Printf("✅ Verified image: %s", imageRef)
	return nil
// 	log.Printf("Signature %s", tag.Signature)
// 	sigBytes, err := base64.StdEncoding.DecodeString(tag.Signature)
// 	if err != nil {
// 		return fmt.Errorf("failed to decode base64 signature: %w", err)
// 	}

// 	// Read and parse the public key
// 	keyBytes, err := os.ReadFile(os.Getenv("COSIGN_PUBLIC_KEY"))
// 	if err != nil {
// 		return fmt.Errorf("failed to read public key: %w", err)
// 	}

// 	pubKey, err := cryptoutils.UnmarshalPEMToPublicKey(keyBytes)
// 	if err != nil {
// 		return fmt.Errorf("failed to parse public key: %w", err)
// 	}

// 	if err := cryptoutils.ValidatePubKey(pubKey); err != nil {
// 		return fmt.Errorf("invalid public key: %w", err)
// 	}

// 	verifier, err := signature.LoadDefaultVerifier(pubKey)

// 	if err != nil {
// 		return fmt.Errorf("failed to create verifier: %w", err)
// 	}

// 	err = verifier.VerifySignature(bytes.NewReader(sigBytes), bytes.NewReader([]byte(tag.Digest)))
// 	if err != nil {
// 		return fmt.Errorf("signature invalid: %w", err)
// 	}

// 	log.Printf("Tag %s verified successfully", tag.Name)
  return nil
}

func processTag(tag Tag) error {
	ready = false
	err := verifyTag(tag)
	if err != nil {
		log.Printf("Error verifying tag %s: %v", tag.Name, err)
		return fmt.Errorf("verification failed for tag %s: %w", tag.Name, err)
	}

	applyTag(tag)

	ready = true
	log.Printf("Tag %s processed successfully", tag.Name)
	return nil
}

func applyTag(tag Tag) error {
	log.Printf("Applying tag: %s (digest: %s)", tag.Name, tag.Digest)

	// Here you would implement the logic to apply the tag.
	// This could involve updating a database, sending a notification, etc.
	// For demonstration purposes, we will just log the action.

	tag.Print()

	// Update the applied state
	repoState.Applied[tag.Name] = tag.Digest
	history.AddEntry("Apply Tag", fmt.Sprintf("Applied tag %s with digest %s", tag.Name, tag.Digest))

	log.Printf("Tag %s applied successfully", tag.Name)
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
