package main

import (
	"bytes"
	"context"
	"crypto"
	"crypto/ecdsa"
	"encoding/base64"
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

	// Artifact signature verfication
	"github.com/google/go-containerregistry/pkg/name"
	"github.com/sigstore/cosign/v2/pkg/oci/remote"
	"github.com/sigstore/sigstore/pkg/cryptoutils"
	"github.com/sigstore/sigstore/pkg/signature"

	// ORAS
	oras "oras.land/oras-go/v2"
	"oras.land/oras-go/v2/content/file"
	orasremote "oras.land/oras-go/v2/registry/remote"

	//  Kubernetes SDK
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"

	"gopkg.in/yaml.v3"
)

type Tag struct {
	Name        string
	Digest      string
	Annotations map[string]string
}

type OCIRepositoryInfo struct {
	LastUpdated time.Time
	Tags        []Tag
	Applied     map[string]string // Map of tagName -> digest
}

func NewOCIRepositoryInfo() OCIRepositoryInfo {
	return OCIRepositoryInfo{
		Tags:    make([]Tag, 0, 10), // Pre-allocate for 10 tags
		Applied: make(map[string]string),
	}
}

func (r *OCIRepositoryInfo) AddTag(tag Tag) {
	// Only store what we need
	r.Tags = append(r.Tags, Tag{Name: tag.Name, Digest: tag.Digest})
}

func (r *OCIRepositoryInfo) MarkApplied(tag Tag) {
	r.Applied[tag.Name] = tag.Digest
}

type TaskHistory struct {
	mu      sync.RWMutex
	entries [100]HistoryEntry
	pos     int // Current position in circular buffer
	count   int // Number of entries
}
type HistoryEntry struct {
	Timestamp time.Time
	Operation string
	Details   string
}

func (h *TaskHistory) AddEntry(operation, details string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.entries[h.pos] = HistoryEntry{
		Timestamp: time.Now(),
		Operation: operation,
		Details:   details,
	}
	h.pos = (h.pos + 1) % len(h.entries)
	if h.count < len(h.entries) {
		h.count++
	}
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

func NewKubeClient() *KubeClient {
	config, err := rest.InClusterConfig()
	if err != nil {
		log.Fatal(err)
	}
	client, err := dynamic.NewForConfig(config)
	if err != nil {
		log.Printf("failed to create dynamic client: %v", err)
		os.Exit(1)
	}
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		log.Printf("failed to create discovery client: %v", err)
		os.Exit(1)
	}
	discoveryClient := memory.NewMemCacheClient(clientset.Discovery())
	discoveryMapper := restmapper.NewDeferredDiscoveryRESTMapper(discoveryClient)
	return &KubeClient{client: client, discoveryMapper: discoveryMapper}
}

func (k *KubeClient) Apply(r io.Reader) error {
	dec := yaml.NewDecoder(r)
	for {
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
		gvk := obj.GroupVersionKind()
		restMapping, err := k.discoveryMapper.RESTMapping(gvk.GroupKind(), gvk.Version)
		if err != nil {
			return err
		}
		gvr := restMapping.Resource
		namespace := obj.GetNamespace()
		if len(namespace) == 0 {
			namespace = "default"
		}
		applyOpts := metav1.ApplyOptions{FieldManager: "kube-apply"}
		log.Printf("⌛ Applying %s resource named '%s' in namespace '%s'", obj.GetKind(), obj.GetName(), obj.GetNamespace())
		_, err = k.client.Resource(gvr).Namespace(namespace).Apply(context.TODO(), obj.GetName(), obj, applyOpts)
		if err != nil {
			return fmt.Errorf("apply error: %w", err)
		}
		log.Printf("⚓ Applied YAML for %s %q", obj.GetKind(), obj.GetName())
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
		time.Sleep(10 * time.Second)
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

	if tags == nil {
		return nil
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
		} else {
			log.Printf("Tag '%s' processed successfully", tag.Name)
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

	if resp.StatusCode == 404 {
		log.Printf("🗙 Repository is not available, sleeping ...")
		time.Sleep(1 * time.Minute)
		return nil, nil
	}

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

		if strings.HasSuffix(tagName, ".sig") || !rules.Matches(tagName) || repoState.Applied[tagName] != "" {
			log.Printf("⚫ Skipping tag '%s' because it doesn't match rules, is a signature, or has already been applied", tagName)
			continue
		}

		log.Printf("⭐ Found tag: '%s'", tagName)
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

	defer resp.Body.Close()

	var manifest struct {
		Annotations map[string]string `json:"annotations"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&manifest); err != nil {
		return Tag{}, fmt.Errorf("error decoding manifest JSON: %v", err)
	}

	return Tag{
		Name:        tagName,
		Digest:      resp.Header.Get("Docker-Content-Digest"),
		Annotations: manifest.Annotations,
	}, nil
}

func verifyTag(tag Tag) error {
	log.Printf("🔍 Verifying tag: %s", tag.Name)

	imageRef := fmt.Sprintf("%s@%s", rules.RepositoryURL, tag.Digest)
	ref, err := name.ParseReference(imageRef, name.Insecure)
	if err != nil {
		return fmt.Errorf("invalid image reference: %w", err)
	}

	base64Data, err := os.ReadFile(os.Getenv("COSIGN_PUBLIC_KEY"))
	if err != nil {
		return fmt.Errorf("failed to read COSIGN_PUBLIC_KEY: %w", err)
	}

	pubKeyBytes := make([]byte, base64.StdEncoding.DecodedLen(len(base64Data)))
	n, err := base64.StdEncoding.Decode(pubKeyBytes, base64Data)
	if err != nil {
		return fmt.Errorf("failed to decode base64 public key: %w", err)
	}
	pubKeyBytes = pubKeyBytes[:n]

	pubKey, err := cryptoutils.UnmarshalPEMToPublicKey(pubKeyBytes)
	if err != nil {
		return fmt.Errorf("failed to parse public key: %w", err)
	}

	ecdsaKey, ok := pubKey.(*ecdsa.PublicKey)
	if !ok {
		return fmt.Errorf("invalid key type, expected ECDSA")
	}

	verifier, err := signature.LoadECDSAVerifier(ecdsaKey, crypto.SHA256)
	if err != nil {
		return fmt.Errorf("failed to create verifier: %w", err)
	}

	entity, err := remote.SignedEntity(ref, remote.WithRemoteOptions())
	if err != nil {
		return fmt.Errorf("failed to fetch remote signed entity: %w", err)
	}

	sigs, err := entity.Signatures()
	if err != nil {
		return fmt.Errorf("failed to extract signatures: %w", err)
	}

	sigList, err := sigs.Get()
	if err != nil {
		return fmt.Errorf("failed to list signatures: %w", err)
	}

	if len(sigList) == 0 {
		return fmt.Errorf("no signatures found for image: %s", imageRef)
	}

	for _, sig := range sigList {
		sigContent, err := sig.Base64Signature()
		if err != nil {
			return fmt.Errorf("failed to get base64 signature: %w", err)
		}
		payload, err := sig.Payload()
		if err != nil {
			return fmt.Errorf("failed to get payload: %w", err)
		}

		sigBytes, err := base64.StdEncoding.DecodeString(sigContent)
		if err != nil {
			return fmt.Errorf("failed to decode signature: %w", err)
		}

		var payloadMap struct {
			Critical struct {
				Image struct {
					Digest string `json:"docker-manifest-digest"`
				} `json:"image"`
			} `json:"critical"`
		}
		if err := json.Unmarshal(payload, &payloadMap); err != nil {
			return fmt.Errorf("failed to parse payload: %w", err)
		}

		signedDigest := payloadMap.Critical.Image.Digest
		fmt.Printf("--> Reference: %s\n", ref.Name())
		fmt.Printf("🔎 Signature base64: %s\n", sigContent)
		log.Printf("📦 Payload inside the digest: %s", signedDigest)
		log.Printf("🔒 Expected digest: %s", tag.Digest)

		if signedDigest != tag.Digest {
			return fmt.Errorf("digest mismatch: expected %s, signed %s", tag.Digest, signedDigest)
		}

		if err := verifier.VerifySignature(bytes.NewReader(sigBytes), bytes.NewReader(payload)); err != nil {
			return fmt.Errorf("signature is invalid: %w", err)
		} else {
			log.Printf("✅ Signature is valid for tag '%s'", tag.Name)
			log.Printf("📝 Annotations:")
			for k, v := range tag.Annotations {
				log.Printf("👀  %s: %s", k, v)
			}
			return nil
		}
	}

	return fmt.Errorf("no valid signatures found for tag %s", tag.Name)
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
		log.Printf("Error applying tag %v", err)
		os.Exit(1)
	}

	ready = true
	log.Printf("✅ Tag '%s' processed successfully", tag.Name)
	return nil
}

func applyTag(tag Tag) error {
	log.Printf("⌛ Applying tag: '%s'", tag.Name)
	fs, err := file.New(filePath)
	if err != nil {
		return fmt.Errorf("error creating file store: %w", err)
	}
	defer fs.Close()
	ctx := context.Background()
	repo, err := orasremote.NewRepository(rules.RepositoryURL)
	repo.PlainHTTP = true // Use plain HTTP for local testing
	if err != nil {
		return fmt.Errorf("error creating repository: %w", err)
	}
	_, err = oras.Copy(ctx, repo, tag.Name, fs, tag.Name, oras.DefaultCopyOptions)
	if err != nil {
		return fmt.Errorf("error copying tag %s from repository: %w", tag.Name, err)
	}

	err = checkAndApplyFiles(filePath)
	if err != nil {
		return fmt.Errorf("error applying file directory: %w", err)
	}
	repoState.MarkApplied(tag)
	history.AddEntry("Apply Tag", fmt.Sprintf("Applied tag %s", tag.Name))

	log.Printf("✅ Tag '%s' applied successfully", tag.Name)
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
			if err := checkAndApplyFiles(subDir); err != nil {
				return err
			}
		} else {
			filePath := dir + "/" + file.Name()

			f, err := os.Open(filePath)
			if err != nil {
				return fmt.Errorf("error opening file %s: %w", filePath, err)
			}
			defer f.Close()
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
		entries: [100]HistoryEntry{},
		pos:     0,
		count:   0,
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
		log.Println("🚀 Kubernetes client initialized")
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
