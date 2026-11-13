package auth

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/ClearedStack/muster/internal/registry"
)

// Where a pod finds the cluster's API and its own credentials. These are
// the standard places; none is specific to this platform.
const (
	serviceAccountDir = "/var/run/secrets/kubernetes.io/serviceaccount"
	reviewPath        = "/apis/authentication.k8s.io/v1/tokenreviews"
)

// Limits of the check.
const (
	reviewTimeout  = 5 * time.Second
	maxReviewBytes = 64 << 10
	cacheFor       = time.Minute
	cacheSize      = 64
)

// Cluster asks the Kubernetes API whose token a caller presented, with a
// TokenReview. It trusts the answer only when the token belongs to the
// ServiceAccount of a component that calls Muster: a ServiceAccount in a
// component's own namespace whose name is that component's registry
// name.
//
// A positive answer is remembered for a minute, by the hash of the
// token, so that a worker's heartbeats do not each cost a call to the
// API. Nothing else is remembered.
type Cluster struct {
	URL       string       // base address of the API
	TokenFile string       // Muster's own token, read on every check
	Client    *http.Client // trusts the cluster's certificate authority
	Clock     func() time.Time

	mu    sync.Mutex
	cache map[[sha256.Size]byte]cached
}

type cached struct {
	slug  string
	until time.Time
}

// InCluster builds a Cluster from the places a pod is given.
func InCluster() (*Cluster, error) {
	host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT")
	if host == "" || port == "" {
		return nil, errors.New("not running in a cluster: the API address is not set")
	}
	pem, err := os.ReadFile(serviceAccountDir + "/ca.crt")
	if err != nil {
		return nil, errors.New("the cluster's certificate authority could not be read")
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pem) {
		return nil, errors.New("the cluster's certificate authority holds no certificate")
	}
	return &Cluster{
		URL:       "https://" + net.JoinHostPort(host, port),
		TokenFile: serviceAccountDir + "/token",
		Client: &http.Client{
			Timeout: reviewTimeout,
			Transport: &http.Transport{
				TLSClientConfig:     &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
				MaxIdleConns:        2,
				MaxIdleConnsPerHost: 2,
				IdleConnTimeout:     90 * time.Second,
			},
		},
		Clock: time.Now,
	}, nil
}

// reviewRequest is the TokenReview Muster sends.
type reviewRequest struct {
	APIVersion string     `json:"apiVersion"`
	Kind       string     `json:"kind"`
	Spec       reviewSpec `json:"spec"`
}

type reviewSpec struct {
	Token string `json:"token"`
}

// reviewAnswer is the part of the cluster's answer Muster reads.
type reviewAnswer struct {
	Status struct {
		Authenticated bool `json:"authenticated"`
		User          struct {
			Username string `json:"username"`
		} `json:"user"`
	} `json:"status"`
}

// Identify asks the cluster whose token this is.
func (c *Cluster) Identify(ctx context.Context, token string) (string, error) {
	if token == "" {
		return "", ErrNoCredential
	}
	key := sha256.Sum256([]byte(token))
	now := c.Clock()
	c.mu.Lock()
	hit, ok := c.cache[key]
	c.mu.Unlock()
	if ok && now.Before(hit.until) {
		return hit.slug, nil
	}

	own, err := os.ReadFile(c.TokenFile)
	if err != nil {
		return "", ErrUnavailable
	}
	body, err := json.Marshal(reviewRequest{
		APIVersion: "authentication.k8s.io/v1",
		Kind:       "TokenReview",
		Spec:       reviewSpec{Token: token},
	})
	if err != nil {
		return "", ErrUnavailable
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL+reviewPath, bytes.NewReader(body))
	if err != nil {
		return "", ErrUnavailable
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(own)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	resp, err := c.Client.Do(req)
	if err != nil {
		return "", ErrUnavailable
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK {
		return "", ErrUnavailable
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxReviewBytes+1))
	if err != nil || len(raw) > maxReviewBytes {
		return "", ErrUnavailable
	}
	var answer reviewAnswer
	if err := json.Unmarshal(raw, &answer); err != nil {
		return "", ErrUnavailable
	}
	if !answer.Status.Authenticated {
		return "", ErrNoCredential
	}
	slug, ok := callerOf(answer.Status.User.Username)
	if !ok {
		return "", ErrNoCredential
	}

	c.mu.Lock()
	if c.cache == nil || len(c.cache) >= cacheSize {
		c.cache = make(map[[sha256.Size]byte]cached, cacheSize)
	}
	c.cache[key] = cached{slug: slug, until: now.Add(cacheFor)}
	c.mu.Unlock()
	return slug, nil
}

// callerOf maps a ServiceAccount's user name to a registry name. The
// user name has the form system:serviceaccount:<namespace>:<name>.
func callerOf(username string) (string, bool) {
	parts := strings.Split(username, ":")
	if len(parts) != 4 || parts[0] != "system" || parts[1] != "serviceaccount" {
		return "", false
	}
	namespace, name := parts[2], parts[3]
	c, ok := registry.Lookup(name)
	if !ok || c.Namespace != namespace || !MayCall(name) {
		return "", false
	}
	return name, true
}
