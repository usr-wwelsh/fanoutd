package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// modelsTTL bounds how stale the catalog can get. The list changes on the order
// of days, so refetching per page load would be pure waste.
const modelsTTL = 30 * time.Minute

var _ Catalog = (*Client)(nil)

type modelsResponse struct {
	Data []struct {
		ID            string `json:"id"`
		Name          string `json:"name"`
		ContextLength int    `json:"context_length"`
		Pricing       struct {
			Prompt     string `json:"prompt"`
			Completion string `json:"completion"`
		} `json:"pricing"`
		SupportedParameters []string `json:"supported_parameters"`
	} `json:"data"`
}

// familyRank orders the free tier by capability. OpenRouter's API exposes no
// ranking field, so this is a hand-kept list of the families that actually
// complete agent runs; anything unlisted sorts after them by context length.
var familyRank = []string{
	"deepseek",
	"qwen3",
	"kimi",
	"glm",
	"minimax",
	"llama-4",
	"gemini",
	"mistral",
	"gpt-oss",
	"ling",
	"nemotron",
	"gemma",
}

// ListModels returns the catalog, free models first and best-ranked first within
// each tier.
func (c *Client) ListModels(ctx context.Context) ([]Model, error) {
	c.mu.Lock()
	fresh := time.Since(c.modelsAt) < modelsTTL
	cached, cachedErr := c.models, c.modelsError
	c.mu.Unlock()
	if fresh && cachedErr == nil && cached != nil {
		return cached, nil
	}

	list, err := c.fetchModels(ctx)

	c.mu.Lock()
	c.modelsAt = time.Now()
	c.modelsError = err
	if err == nil {
		c.models = list
	}
	c.mu.Unlock()

	if err != nil {
		// A stale catalog beats an empty picker.
		if cached != nil {
			return cached, nil
		}
		return nil, err
	}
	return list, nil
}

func (c *Client) fetchModels(ctx context.Context) ([]Model, error) {
	req, err := http.NewRequestWithContext(ctx, "GET", c.BaseURL+"/models", nil)
	if err != nil {
		return nil, err
	}
	c.authorize(req)

	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s models error %d: %s", c.name(), resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var parsed modelsResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, err
	}

	out := make([]Model, 0, len(parsed.Data))
	for _, m := range parsed.Data {
		if m.ID == "" {
			continue
		}
		name := m.Name
		if name == "" {
			name = m.ID
		}
		out = append(out, Model{
			ID:            m.ID,
			Name:          name,
			ContextLength: m.ContextLength,
			Free:          isZeroPrice(m.Pricing.Prompt) && isZeroPrice(m.Pricing.Completion),
			Tools:         contains(m.SupportedParameters, "tools"),
		})
	}

	sortModels(out)
	return out, nil
}

// sortModels puts free models first, then ranked families, then tool-capable
// models, then the largest context window.
func sortModels(list []Model) {
	sort.SliceStable(list, func(i, j int) bool {
		a, b := list[i], list[j]
		if a.Free != b.Free {
			return a.Free
		}
		ra, rb := rankOf(a.ID), rankOf(b.ID)
		if ra != rb {
			return ra < rb
		}
		if a.Tools != b.Tools {
			return a.Tools
		}
		if a.ContextLength != b.ContextLength {
			return a.ContextLength > b.ContextLength
		}
		return a.ID < b.ID
	})
}

func rankOf(id string) int {
	id = strings.ToLower(id)
	for i, family := range familyRank {
		if strings.Contains(id, family) {
			return i
		}
	}
	return len(familyRank)
}

func isZeroPrice(v string) bool {
	f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
	return err == nil && f == 0
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}
