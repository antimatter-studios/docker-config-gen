package management

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// Client communicates with the docker-proxy management server over a Unix socket.
type Client struct {
	httpClient *http.Client
	baseURL    string
}

// NewClient creates a management client connected to the given Unix socket path.
func NewClient(socketPath string) *Client {
	return &Client{
		httpClient: &http.Client{
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					var d net.Dialer
					d.Timeout = 5 * time.Second
					return d.DialContext(ctx, "unix", socketPath)
				},
			},
			Timeout: 30 * time.Second,
		},
		// The host is ignored for Unix sockets but required by HTTP
		baseURL: "http://proxy",
	}
}

// GetTemplate retrieves the nginx template from the management server.
func (c *Client) GetTemplate(ctx context.Context) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/template", nil)
	if err != nil {
		return "", fmt.Errorf("creating request: %w", err)
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("requesting template: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return "", fmt.Errorf("template request failed (status %d): %s", resp.StatusCode, string(body))
	}

	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("reading template response: %w", err)
	}

	return string(data), nil
}

// SendConfig sends the rendered configuration to the management server.
// An empty config triggers a reset to the default configuration.
func (c *Client) SendConfig(ctx context.Context, config string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/config", strings.NewReader(config))
	if err != nil {
		return fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Content-Type", "text/plain")

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("sending config: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("config update failed (status %d): %s", resp.StatusCode, string(body))
	}

	return nil
}

// Health checks if the management server is reachable.
func (c *Client) Health(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/health", nil)
	if err != nil {
		return err
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("health check failed: status %d", resp.StatusCode)
	}
	return nil
}
