package node

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// Node is the part of a Kubernetes Node object that the installer reads.
type Node struct {
	RuntimeVersion string // For example "containerd://2.2.1".
	// MaxPods is the node's pod capacity, the kubelet's maxPods.
	MaxPods int
}

// API reads and patches the installer's own Node object.
type API interface {
	Node(ctx context.Context) (Node, error)
	// Patch sets labels and annotations. A nil value removes the key.
	Patch(ctx context.Context, labels, annotations map[string]*string) error
}

const serviceAccountDir = "/var/run/secrets/kubernetes.io/serviceaccount"

// client talks to the API server with the pod's service account, without client-go.
type client struct {
	url  string
	http *http.Client
}

// InCluster returns an API for the node name from inside a pod.
func InCluster(name string) (API, error) {
	ca, err := os.ReadFile(serviceAccountDir + "/ca.crt")
	if err != nil {
		return nil, fmt.Errorf("read service account CA: %w", err)
	}

	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return nil, errors.New("service account CA has no certificate")
	}

	host := net.JoinHostPort(os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT"))
	transport := http.DefaultTransport.(*http.Transport).Clone()                         //nolint:errcheck,forcetypeassert // The default transport is an *http.Transport.
	transport.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12} //nolint:exhaustruct_v5 // Defaults for the rest.

	return &client{
		url:  "https://" + host + "/api/v1/nodes/" + name,
		http: &http.Client{Transport: transport, Timeout: 30 * time.Second},
	}, nil
}

func (c *client) Node(ctx context.Context) (Node, error) {
	var obj struct {
		Status struct {
			NodeInfo struct {
				ContainerRuntimeVersion string `json:"containerRuntimeVersion"`
			} `json:"nodeInfo"`
			Capacity struct {
				Pods string `json:"pods"`
			} `json:"capacity"`
		} `json:"status"`
	}

	body, err := c.do(ctx, http.MethodGet, nil)
	if err != nil {
		return Node{}, err
	}

	err = json.Unmarshal(body, &obj)
	if err != nil {
		return Node{}, fmt.Errorf("decode node: %w", err)
	}

	maxPods, err := strconv.Atoi(obj.Status.Capacity.Pods)
	if err != nil {
		return Node{}, fmt.Errorf("decode node pod capacity %q: %w", obj.Status.Capacity.Pods, err)
	}

	return Node{RuntimeVersion: obj.Status.NodeInfo.ContainerRuntimeVersion, MaxPods: maxPods}, nil
}

func (c *client) Patch(ctx context.Context, labels, annotations map[string]*string) error {
	patch, err := json.Marshal(map[string]any{"metadata": map[string]any{"labels": labels, "annotations": annotations}})
	if err != nil {
		return fmt.Errorf("encode node patch: %w", err)
	}

	_, err = c.do(ctx, http.MethodPatch, patch)

	return err
}

func (c *client) do(ctx context.Context, method string, body []byte) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.url, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}

	// The kubelet rotates the token, so read it for every request.
	token, err := os.ReadFile(serviceAccountDir + "/token")
	if err != nil {
		return nil, fmt.Errorf("read service account token: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
	req.Header.Set("Content-Type", "application/merge-patch+json")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("API server: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck // The body is read in full.

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("read response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("API server: %s: %s", resp.Status, bytes.TrimSpace(data))
	}

	return data, nil
}
