// Package jev is a minimal client for TypeSafe's System One endpoint (POST /v1/systemone).
// There is no Go SDK; the shapes follow https://docs.typesafe.ai/api.
package jev

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const DefaultBaseURL = "https://api.typesafe.ai"

type Client struct {
	baseURL string
	apiKey  string
	model   string
	http    *http.Client
}

func New(baseURL, apiKey, model string) *Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.MaxIdleConnsPerHost = 4 // keep a warm connection; TLS setup is a big share of latency
	// All requests share one HTTP/2 connection. Without health checks, a connection that
	// silently dies makes every later call hang until its timeout. Ping a quiet
	// connection and drop it when the ping goes unanswered.
	tr.HTTP2 = &http.HTTP2Config{SendPingTimeout: 15 * time.Second, PingTimeout: 2 * time.Second}
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		apiKey:  apiKey,
		model:   model,
		http:    &http.Client{Transport: tr},
	}
}

// Question is a Choice, Noul, or Score question. Instructions and criteria may be strings
// or structured values.
type Question struct {
	Type         string `json:"type"` // "choice" | "noul" | "score"
	Instructions any    `json:"instructions"`
	Criteria     any    `json:"criteria,omitempty"`
}

func Choice(instructions any, criteria map[string]any) Question {
	return Question{Type: "choice", Instructions: instructions, Criteria: criteria}
}

func Noul(instructions any) Question {
	return Question{Type: "noul", Instructions: instructions}
}

type Answer struct {
	Type          string             `json:"type"`
	Choice        string             `json:"choice,omitempty"`
	Noul          float64            `json:"noul,omitempty"`
	Score         float64            `json:"score,omitempty"`
	Probabilities map[string]float64 `json:"probabilities,omitempty"`
	Confidence    float64            `json:"confidence,omitempty"`
}

type Response struct {
	Model   string            `json:"model"`
	Answers map[string]Answer `json:"answers"`
	Usage   struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

// APIError is a non-200 response.
type APIError struct {
	Status int
	Body   string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("jev: HTTP %d: %s", e.Status, e.Body)
}

// Evaluate asks every question against state in one request. Questions are evaluated in
// parallel server-side, so speculative extras cost tokens but little latency.
func (c *Client) Evaluate(ctx context.Context, state any, questions map[string]Question) (*Response, error) {
	body, err := json.Marshal(map[string]any{
		"model":     c.model,
		"state":     state,
		"questions": questions,
	})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/systemone", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		// Start the next call on a fresh connection in case this one is wedged.
		c.http.CloseIdleConnections()
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, &APIError{Status: resp.StatusCode, Body: string(raw)}
	}
	var out Response
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("jev: decoding response: %w", err)
	}
	return &out, nil
}

// Warm opens a connection ahead of the first real request (errors are ignored).
func (c *Client) Warm() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/v1/models", nil)
	if err != nil {
		return
	}
	req.Header.Set("Authorization", "Bearer "+c.apiKey)
	if resp, err := c.http.Do(req); err == nil {
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
}
