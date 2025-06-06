package main

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"encoding/json"
	"errors"
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

	"github.com/google/go-containerregistry/pkg/name"
	"github.com/sigstore/cosign/v2/pkg/oci/remote"
	"github.com/sigstore/sigstore/pkg/cryptoutils"
	"github.com/sigstore/sigstore/pkg/signature"

	oras "oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content/file"
	orasremote "oras.land/oras-go/v2/registry/remote"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"

	"gopkg.in/yaml.v3"
)

// Tag represents a repository tag with metadata
type Tag struct {
	Name        string
	Digest      string
	ContentType string
}

func (t Tag) Print() {
	log.Printf("Tag: %s, Content Type: %s",
		t.Name, t.ContentType)
}

// OCIRepositoryInfo represents information about a repository
type OCIRepositoryInfo struct {
	LastUpdated time.Time
	Tags        []Tag             // All available tags
	Applied     map[string][]byte // Map of tagName -> digest for applied tags
}

func NewOCIRepositoryInfo() OCIRepositoryInfo {
	return OCIRepositoryInfo{
		Tags:    make([]Tag, 0),
		Applied: make(map[string][]byte),
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

type KubeClient struct {
    client          *dynamic.DynamicClient
    discoveryMapper *restmapper.DeferredDiscoveryRESTMapper
}
// NewKubeClient creates an instance of KubeClient
func NewKubeClient() *KubeClient {
    config, err := rest.InClusterConfig()
    if err != nil {
        log.Fatal(err)
    }
    // create the dynamic client
    client, err := dynamic.NewForConfig(config)
    if err != nil {
        log.Fatalf("failed to create dynamic client: %w", err)
    }
    // create a discovery client to map dynamic API resources
    clientset, err := kubernetes.NewForConfig(config)
    if err != nil {
        log.Fatalf("failed to create discovery client: %w", err)
    }
    discoveryClient := memory.NewMemCacheClient(clientset.Discovery())
    discoveryMapper := restmapper.NewDeferredDiscoveryRESTMapper(discoveryClient)
    return &KubeClient{client: client, discoveryMapper: discoveryMapper}
}
// Apply applies the given YAML manifests to kubernetes
func (k *KubeClient) Apply(r io.Reader) error {
    dec := yaml.NewDecoder(r)
    for {
        // parse the YAML doc
        obj := &unstructured.Unstructured{Object: map[string]interface{}{}}
        err := dec.Decode(obj.Object)
        if errors.Is(err, io.EOF) {
            break
        }
        if err != nil {
            return err
        }
        if obj.Object == nil {
            log.Print("skipping empty document")
            continue
        }
        // get GroupVersionResource to invoke the dynamic client
        gvk := obj.GroupVersionKind()
        restMapping, err := k.discoveryMapper.RESTMapping(gvk.GroupKind(), gvk.Version)
        if err != nil {
            return err
        }
        gvr := restMapping.Resource
        // apply the YAML doc
        namespace := obj.GetNamespace()
        if len(namespace) == 0 {
            namespace = "default"
        }
        applyOpts := metav1.ApplyOptions{FieldManager: "kube-apply"}
		log.Printf("Applying %s named %s in namespace %s", obj.GetKind(), obj.GetName(), obj.GetNamespace())
        _, err = k.client.Resource(gvr).Namespace(namespace).Apply(context.TODO(), obj.GetName(), obj, applyOpts)
        if err != nil {
            return fmt.Errorf("apply error: %w", err)
        }
        log.Printf("applied YAML for %s %q", obj.GetKind(), obj.GetName())
    }
    return nil
}

var (
	filePath   string
	rules      *Rules
	rules_file string
	ready      bool
	repoState  OCIRepositoryInfo
	stopChan   chan struct{}
	history    TaskHistory
	kubeClient *KubeClient
)

func (r *Rules) checkRepository() error {
	for !ready {
		log.Printf("Waiting for readiness...")
		time.Sleep(10*time.Second)
	}
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
		

		tag_index := -1
		for i, t := range repoState.Tags {
			if t.Name == tag.Name {
				tag_index = i
				break
			}
		}
		if tag_index != -1 {
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

		if strings.HasSuffix(tagName, ".sig") || !rules.Matches(tagName) || repoState.Applied[tagName] != nil {
			log.Printf("Skipping tag %s because it doesn't match rules, is a signature, or has already been applied", tagName)
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
	// Now get the main artifact content
	manifestURL := fmt.Sprintf("http://%s/v2/%s/manifests/%s", registry, repository, tagName)
	req, err := http.NewRequest("GET", manifestURL, nil)
	if err != nil {
		return Tag{}, fmt.Errorf("error creating request: %v", err)
	}

	req.Header.Add("Accept", "application/vnd.oci.image.manifest.v1+json")
	req.Header.Add("Accept", "application/vnd.docker.distribution.manifest.v2+json")

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return Tag{}, fmt.Errorf("error fetching manifest: %v", err)
	}

	if resp.StatusCode != http.StatusOK {
		return Tag{}, fmt.Errorf("unexpected status code: %d", resp.StatusCode)
	}

	contentType := resp.Header.Get("Content-Type")

	return Tag{
		Name:        tagName,
		ContentType: contentType,
		Digest:      resp.Header.Get("Docker-Content-Digest"),
	}, nil
}

func verifyTag(tag Tag) error {
	log.Printf("Verifying tag: %s", tag.Name)
	imageRef := fmt.Sprintf("%s:%s", rules.RepositoryURL, tag.Name)
	log.Printf("Image reference to verify: %s", imageRef)
	// Parse the image reference
	ref, err := name.ParseReference(imageRef, name.Insecure)
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

	ecdsaPubKey, ok := pubKey.(*ecdsa.PublicKey)
	if !ok {
		return fmt.Errorf("public key is not an ECDSA key")
	}
	_, err = signature.LoadECDSAVerifier(ecdsaPubKey, crypto.SHA256)
	if err != nil {
		return fmt.Errorf("creating verifier: %w", err)
	}

	entity, err := remote.SignedEntity(ref, remote.WithRemoteOptions())
	if err != nil {
		return fmt.Errorf("failed to get signed entity for reference: %w", err)
	}
	signaturesE, err := entity.Signatures()
	if err != nil {
		return fmt.Errorf("failed to get signatures for entity: %w", err)
	}
	signatures, err := signaturesE.Get()
	for _, sig := range signatures {
		payload, err := sig.Payload()
		if err != nil {
			return fmt.Errorf("failed to get payload for signature: %w", err)
		}
		// Payload extract the field digest from JSON payload .critical.image.docker-manifest-digest
		var payloadMap map[string]interface{}
		if err := json.Unmarshal(payload, &payloadMap); err != nil {
			return fmt.Errorf("failed to unmarshal payload: %w", err)
		}
		digest, ok := payloadMap["critical"].(map[string]interface{})["image"].(map[string]interface{})["docker-manifest-digest"].(string)
		if !ok {
			return fmt.Errorf("digest not found in payload")
		}
		log.Printf("Digest found in payload: %s", digest)
		if digest != tag.Digest {
			return fmt.Errorf("digest mismatch: expected %s, got %s", tag.Digest, digest)
		}
		log.Printf("Digest matches for tag: %s", tag.Name)
	}

	log.Printf("✅ Verified image: %s", imageRef)
	return nil
}

func processTag(tag Tag) error {
	ready = false
	err := verifyTag(tag)
	if err != nil {
		log.Printf("Error verifying tag %s: %v", tag.Name, err)
		return fmt.Errorf("verification failed for tag %s: %w", tag.Name, err)
	}

	err = applyTag(tag)
	if err != nil {
		log.Printf("Error applying tag %w", err)
		os.Exit(1)
	}

	ready = true
	log.Printf("Tag %s processed successfully", tag.Name)
	return nil
}

func applyTag(tag Tag) error {
	log.Printf("Applying tag: %s", tag.Name)

	// 0. Create a file store
	fs, err := file.New(filePath)
	if err != nil {
		return fmt.Errorf("error creating file store: %w", err)
	}
	defer fs.Close()

	// 1. Connect to the remote repository
	ctx := context.Background()
	repo, err := orasremote.NewRepository(rules.RepositoryURL)
	repo.PlainHTTP = true // Use plain HTTP for local testing
	if err != nil {
		return fmt.Errorf("error creating repository: %w", err)
	}

	// 2. Copy from the remote repository to the file store
	_, err = oras.Copy(ctx, repo, tag.Name, fs, tag.Name, oras.DefaultCopyOptions)
	if err != nil {
		return fmt.Errorf("error copying tag %s from repository: %w", tag.Name, err)
	}

	err = checkAndApplyFiles(filePath)
	if err != nil {
		return fmt.Errorf("error applying file directory: %w", err)
	}
	repoState.Applied[tag.Name] = []byte(tag.Digest)
	history.AddEntry("Apply Tag", fmt.Sprintf("Applied tag %s", tag.Name))

	log.Printf("Tag %s applied successfully", tag.Name)
	return nil
}

func checkAndApplyFiles(dir string) error {
	files, err := os.ReadDir(dir)
	if err != nil {
		return fmt.Errorf("error reading directory %s: %w", dir, err)
	}

	for _, file := range files {
		if file.IsDir() {
			subDir := dir + "/" + file.Name()
			return checkAndApplyFiles(subDir)
		} else {
			filePath := dir + "/" + file.Name()
			
			f, err := os.Open(filePath)
			if err != nil {
				return fmt.Errorf("error opening file %s: %w", filePath, err)
			}
			defer f.Close()
			log.Printf("Applying file: %s", filePath)
			if err := kubeClient.Apply(f); err != nil {
				return fmt.Errorf("error applying file %s: %w", filePath, err)
			}
		}
	}

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

	filePath = os.Getenv("FILE_STORE_PATH")

	parseRulesFile()
	history.AddEntry("Startup", "Agent initialized with rules file: "+rules_file)
	go func() {
		kubeClient = NewKubeClient()
		if kubeClient == nil {
			log.Fatal("Failed to create Kubernetes client")
		}
		log.Println("Kubernetes client initialized")
	}()
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
