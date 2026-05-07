package memory

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"
)

// ChromaClient wraps the ChromaDB HTTP API.
type ChromaClient struct {
	BaseURL      string
	HTTP         *http.Client
	collection   string // collection UUID (not name)
	collectionName string
}

// NewChromaClient creates a new ChromaDB client.
func NewChromaClient(baseURL, collectionName string) *ChromaClient {
	return &ChromaClient{
		BaseURL:        baseURL,
		HTTP:           &http.Client{Timeout: 15 * time.Second},
		collectionName: collectionName,
	}
}

// EnsureCollection creates the collection if it doesn't exist, stores its UUID.
func (c *ChromaClient) EnsureCollection() error {
	// Try to find existing collection
	uuid, err := c.findCollection()
	if err == nil && uuid != "" {
		c.collection = uuid
		slog.Info("found existing collection", "name", c.collectionName, "uuid", uuid)
		return nil
	}

	// Create collection
	body := map[string]interface{}{
		"name":     c.collectionName,
		"metadata": map[string]string{"description": "DingDing bot long-term memories"},
	}
	data, _ := json.Marshal(body)

	resp, err := c.HTTP.Post(c.BaseURL+"/api/v2/tenants/default_tenant/databases/default_database/collections",
		"application/json", bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("create collection: %w", err)
	}
	defer resp.Body.Close()

	respData, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return fmt.Errorf("create collection %d: %s", resp.StatusCode, string(respData))
	}

	// Parse the created collection's UUID
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(respData, &created); err == nil && created.ID != "" {
		c.collection = created.ID
		slog.Info("created collection", "name", c.collectionName, "uuid", c.collection)
	}
	return nil
}

func (c *ChromaClient) findCollection() (string, error) {
	resp, err := c.HTTP.Get(c.BaseURL + "/api/v2/tenants/default_tenant/databases/default_database/collections")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	respData, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("list collections %d: %s", resp.StatusCode, string(respData))
	}

	var collections []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(respData, &collections); err != nil {
		return "", fmt.Errorf("parse collections: %w", err)
	}

	for _, col := range collections {
		if col.Name == c.collectionName {
			return col.ID, nil
		}
	}
	return "", fmt.Errorf("collection %q not found", c.collectionName)
}

// url builds a collection-specific URL.
func (c *ChromaClient) url(path string) string {
	return fmt.Sprintf("%s/api/v2/tenants/default_tenant/databases/default_database/collections/%s%s",
		c.BaseURL, c.collection, path)
}

// AddMemory stores a fact with its embedding and metadata.
func (c *ChromaClient) AddMemory(factID, content string, embedding []float64, metadata map[string]interface{}) error {
	body := map[string]interface{}{
		"ids":        []string{factID},
		"embeddings": [][]float64{embedding},
		"documents":  []string{content},
		"metadatas":  []map[string]interface{}{metadata},
	}
	data, _ := json.Marshal(body)

	resp, err := c.HTTP.Post(c.url("/add"), "application/json", bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("add memory: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		respData, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("add memory %d: %s", resp.StatusCode, string(respData))
	}
	return nil
}

// SearchMemory finds the top-k most similar memories to the query embedding.
func (c *ChromaClient) SearchMemory(queryEmbedding []float64, k int) ([]SearchResult, error) {
	body := map[string]interface{}{
		"query_embeddings": [][]float64{queryEmbedding},
		"n_results":        k,
	}
	data, _ := json.Marshal(body)

	resp, err := c.HTTP.Post(c.url("/query"), "application/json", bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("search memory: %w", err)
	}
	defer resp.Body.Close()

	respData, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("search memory %d: %s", resp.StatusCode, string(respData))
	}

	var result struct {
		Documents [][]string                 `json:"documents"`
		Metadatas [][]map[string]interface{} `json:"metadatas"`
		Distances [][]float64                `json:"distances"`
		IDs       [][]string                 `json:"ids"`
	}
	if err := json.Unmarshal(respData, &result); err != nil {
		return nil, fmt.Errorf("parse search result: %w", err)
	}

	var memories []SearchResult
	if len(result.Documents) > 0 {
		for i, doc := range result.Documents[0] {
			m := SearchResult{Document: doc}
			if len(result.IDs) > 0 && i < len(result.IDs[0]) {
				m.ID = result.IDs[0][i]
			}
			if len(result.Distances) > 0 && i < len(result.Distances[0]) {
				m.Distance = result.Distances[0][i]
			}
			if len(result.Metadatas) > 0 && i < len(result.Metadatas[0]) {
				m.Metadata = result.Metadatas[0][i]
			}
			memories = append(memories, m)
		}
	}
	return memories, nil
}

// SearchResult is a single memory search result.
type SearchResult struct {
	ID       string
	Document string
	Distance float64
	Metadata map[string]interface{}
}

// DeleteMemory removes a memory by ID.
func (c *ChromaClient) DeleteMemory(factID string) error {
	body := map[string]interface{}{
		"ids": []string{factID},
	}
	data, _ := json.Marshal(body)

	resp, err := c.HTTP.Post(c.url("/delete"), "application/json", bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("delete memory: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respData, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("delete memory %d: %s", resp.StatusCode, string(respData))
	}
	return nil
}

// ListMemories returns recent memories from the collection.
func (c *ChromaClient) ListMemories(limit int) []SearchResult {
	body := map[string]interface{}{
		"limit": limit,
		"offset": 0,
	}
	data, _ := json.Marshal(body)

	resp, err := c.HTTP.Post(c.url("/get"), "application/json", bytes.NewReader(data))
	if err != nil {
		slog.Warn("list memories failed", "error", err)
		return nil
	}
	defer resp.Body.Close()

	respData, _ := io.ReadAll(resp.Body)
	var result struct {
		Documents []string                   `json:"documents"`
		Metadatas []map[string]interface{}   `json:"metadatas"`
		IDs       []string                   `json:"ids"`
	}
	if err := json.Unmarshal(respData, &result); err != nil {
		slog.Warn("parse memories", "error", err)
		return nil
	}

	var memories []SearchResult
	for i, doc := range result.Documents {
		m := SearchResult{ID: result.IDs[i], Document: doc}
		if i < len(result.Metadatas) {
			m.Metadata = result.Metadatas[i]
		}
		memories = append(memories, m)
	}
	return memories
}
