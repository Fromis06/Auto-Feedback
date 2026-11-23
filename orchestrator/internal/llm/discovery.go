package llm

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// ModelPreferences specifies the desired model names or "auto"/"" for auto-discovery.
type ModelPreferences struct {
	Embedding string
	Triage    string
	Coding    string
}

// ResolvedModels holds the concrete model IDs for all three orchestrator/sandbox roles.
type ResolvedModels struct {
	Embedding string
	Triage    string
	Coding    string
}

const (
	DefaultFallbackEmbeddingModel = "text-embedding-3-small"
	DefaultFallbackTriageModel    = "gpt-4o-mini"
	DefaultFallbackCodingModel    = "gpt-4o"
)

// IsAutoModel reports whether a configured model value requests auto-discovery.
func IsAutoModel(v string) bool {
	trimmed := strings.TrimSpace(v)
	return trimmed == "" || strings.EqualFold(trimmed, "auto")
}

// NeedsDiscovery returns true if at least one role is set to "auto" or empty.
func (p ModelPreferences) NeedsDiscovery() bool {
	return IsAutoModel(p.Embedding) || IsAutoModel(p.Triage) || IsAutoModel(p.Coding)
}

type modelsListResponse struct {
	Data []struct {
		ID string `json:"id"`
	} `json:"data"`
	Models []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"models"`
}

// DiscoverModels queries GET {baseURL}/models and classifies available models into
// Embedding, Triage, and Coding roles for any field left as "auto" or empty.
func DiscoverModels(ctx context.Context, baseURL, apiKey string, prefs ModelPreferences) (ResolvedModels, error) {
	resolved := ResolvedModels{
		Embedding: strings.TrimSpace(prefs.Embedding),
		Triage:    strings.TrimSpace(prefs.Triage),
		Coding:    strings.TrimSpace(prefs.Coding),
	}
	if !prefs.NeedsDiscovery() {
		return resolved, nil
	}

	ids, err := fetchAvailableModels(ctx, baseURL, apiKey)
	if err != nil {
		return resolved, err
	}
	if len(ids) == 0 {
		return resolved, fmt.Errorf("models endpoint %s/models returned no models", strings.TrimRight(baseURL, "/"))
	}

	discovered := SelectModelsFromList(ids)
	if IsAutoModel(resolved.Embedding) {
		resolved.Embedding = discovered.Embedding
	}
	if IsAutoModel(resolved.Triage) {
		resolved.Triage = discovered.Triage
	}
	if IsAutoModel(resolved.Coding) {
		resolved.Coding = discovered.Coding
	}

	return resolved, nil
}

// ApplyFallbacks replaces any remaining "auto" or empty model slots with safe defaults.
func (r ResolvedModels) ApplyFallbacks() ResolvedModels {
	if IsAutoModel(r.Embedding) {
		r.Embedding = DefaultFallbackEmbeddingModel
	}
	if IsAutoModel(r.Triage) {
		r.Triage = DefaultFallbackTriageModel
	}
	if IsAutoModel(r.Coding) {
		r.Coding = DefaultFallbackCodingModel
	}
	return r
}

func fetchAvailableModels(ctx context.Context, baseURL, apiKey string) ([]string, error) {
	endpoint := strings.TrimRight(strings.TrimSpace(baseURL), "/") + "/models"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return nil, fmt.Errorf("create models request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if strings.TrimSpace(apiKey) != "" {
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(apiKey))
	}

	httpClient := &http.Client{Timeout: 15 * time.Second}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch models from %s: %w", endpoint, err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 5<<20))
	if err != nil {
		return nil, fmt.Errorf("read models response: %w", err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("models endpoint returned status %d: %s", resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var parsed modelsListResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("decode models response: %w", err)
	}

	seen := make(map[string]struct{})
	var ids []string
	add := func(id string) {
		id = strings.TrimSpace(id)
		if id == "" {
			return
		}
		if _, ok := seen[id]; ok {
			return
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}

	for _, m := range parsed.Data {
		add(m.ID)
	}
	for _, m := range parsed.Models {
		if m.ID != "" {
			add(m.ID)
		} else {
			add(m.Name)
		}
	}
	return ids, nil
}

// SelectModelsFromList scores a list of model IDs and chooses the best fit for
// Embedding, Triage, and Coding roles.
func SelectModelsFromList(modelIDs []string) ResolvedModels {
	var embeddingCandidates []scoredModel
	var chatCandidates []string

	for _, id := range modelIDs {
		trimmed := strings.TrimSpace(id)
		if trimmed == "" {
			continue
		}
		if isEmbeddingModel(trimmed) {
			embeddingCandidates = append(embeddingCandidates, scoredModel{
				id:    trimmed,
				score: scoreEmbeddingModel(trimmed),
			})
			continue
		}
		if isNonChatUtilityModel(trimmed) {
			continue
		}
		chatCandidates = append(chatCandidates, trimmed)
	}

	var result ResolvedModels

	if len(embeddingCandidates) > 0 {
		sortScoredModels(embeddingCandidates)
		result.Embedding = embeddingCandidates[0].id
	}

	if len(chatCandidates) > 0 {
		triageCandidates := make([]scoredModel, 0, len(chatCandidates))
		codingCandidates := make([]scoredModel, 0, len(chatCandidates))
		for _, id := range chatCandidates {
			triageCandidates = append(triageCandidates, scoredModel{
				id:    id,
				score: scoreTriageModel(id),
				size:  extractParamBillions(id),
			})
			codingCandidates = append(codingCandidates, scoredModel{
				id:    id,
				score: scoreCodingModel(id),
				size:  extractParamBillions(id),
			})
		}

		// For triage, prefer higher score, then smaller parameter count.
		sort.SliceStable(triageCandidates, func(i, j int) bool {
			if triageCandidates[i].score != triageCandidates[j].score {
				return triageCandidates[i].score > triageCandidates[j].score
			}
			if triageCandidates[i].size != triageCandidates[j].size {
				if triageCandidates[i].size == 0 {
					return false
				}
				if triageCandidates[j].size == 0 {
					return true
				}
				return triageCandidates[i].size < triageCandidates[j].size
			}
			return triageCandidates[i].id < triageCandidates[j].id
		})
		result.Triage = triageCandidates[0].id

		// For coding, prefer higher score, then larger parameter count.
		sort.SliceStable(codingCandidates, func(i, j int) bool {
			if codingCandidates[i].score != codingCandidates[j].score {
				return codingCandidates[i].score > codingCandidates[j].score
			}
			if codingCandidates[i].size != codingCandidates[j].size {
				return codingCandidates[i].size > codingCandidates[j].size
			}
			return codingCandidates[i].id < codingCandidates[j].id
		})
		result.Coding = codingCandidates[0].id
	}

	return result
}

type scoredModel struct {
	id    string
	score int
	size  float64
}

func sortScoredModels(items []scoredModel) {
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].score != items[j].score {
			return items[i].score > items[j].score
		}
		return items[i].id < items[j].id
	})
}

func normalizeModelName(id string) string {
	low := strings.ToLower(strings.TrimSpace(id))
	low = strings.TrimPrefix(low, "models/")
	return low
}

func isEmbeddingModel(id string) bool {
	low := normalizeModelName(id)
	for _, kw := range []string{"embed", "nomic", "bge", "mxbai"} {
		if strings.Contains(low, kw) {
			return true
		}
	}
	return false
}

func isNonChatUtilityModel(id string) bool {
	low := normalizeModelName(id)
	for _, kw := range []string{
		"whisper", "tts", "dall-e", "imagen", "moderation", "transcribe",
		"audio-preview", "realtime", "sora", "veo", "aqa", "rerank", "babbage", "davinci",
	} {
		if strings.Contains(low, kw) {
			return true
		}
	}
	return false
}

func scoreEmbeddingModel(id string) int {
	low := normalizeModelName(id)
	score := 10
	switch {
	case strings.Contains(low, "text-embedding-004"):
		score += 120
	case strings.Contains(low, "nomic-embed-text"):
		score += 115
	case strings.Contains(low, "text-embedding-3-small"):
		score += 110
	case strings.Contains(low, "gemini-embedding"):
		score += 100
	case strings.Contains(low, "mxbai-embed"):
		score += 95
	case strings.Contains(low, "bge"):
		score += 90
	case strings.Contains(low, "text-embedding-3-large"):
		score += 85
	case strings.Contains(low, "embedding"):
		score += 70
	case strings.Contains(low, "embed"):
		score += 65
	case strings.Contains(low, "nomic"):
		score += 60
	}
	if strings.Contains(low, "latest") {
		score += 5
	}
	return score
}

var reModelTokens = regexp.MustCompile(`[^a-z0-9.]+`)
var reParamBillions = regexp.MustCompile(`(?:^|[^a-z0-9])(\d+(?:\.\d+)?)b(?:$|[^a-z0-9])`)

func hasToken(low, token string) bool {
	for _, part := range reModelTokens.Split(low, -1) {
		if part == token {
			return true
		}
	}
	return false
}

func extractParamBillions(id string) float64 {
	low := normalizeModelName(id)
	matches := reParamBillions.FindAllStringSubmatch(low, -1)
	var maxVal float64
	for _, m := range matches {
		if len(m) < 2 {
			continue
		}
		if v, err := strconv.ParseFloat(m[1], 64); err == nil && v > maxVal {
			maxVal = v
		}
	}
	return maxVal
}

func scoreTriageModel(id string) int {
	low := normalizeModelName(id)
	score := 10

	if strings.Contains(low, "flash") {
		score += 100
	}
	if strings.Contains(low, "mini") {
		score += 98
	}
	if strings.Contains(low, "haiku") {
		score += 96
	}
	if strings.Contains(low, "nano") {
		score += 94
	}
	if strings.Contains(low, "lite") || hasToken(low, "small") || hasToken(low, "fast") || hasToken(low, "instant") {
		score += 60
	}

	params := extractParamBillions(low)
	switch {
	case params > 0 && params <= 4:
		score += 90
	case params > 4 && params <= 8:
		score += 70
	case params >= 30:
		score -= 40
	}

	// Prefer general-purpose lightweight models over specialized coding or flagship reasoning models.
	if strings.Contains(low, "coder") || strings.Contains(low, "codestral") || strings.Contains(low, "starcoder") {
		score -= 45
	}
	if hasToken(low, "pro") || strings.Contains(low, "opus") || strings.Contains(low, "sonnet") {
		score -= 35
	}
	if strings.Contains(low, "gpt-4o") && !strings.Contains(low, "mini") {
		score -= 30
	}
	if strings.Contains(low, "latest") {
		score += 2
	}

	return score
}

func scoreCodingModel(id string) int {
	low := normalizeModelName(id)
	score := 10

	if strings.Contains(low, "coder") || strings.Contains(low, "codestral") || strings.Contains(low, "starcoder") {
		score += 130
	}
	if strings.Contains(low, "opus") {
		score += 120
	}
	if strings.Contains(low, "sonnet") {
		score += 115
	}
	if strings.Contains(low, "gpt-4o") && !strings.Contains(low, "mini") {
		score += 115
	}
	if (strings.Contains(low, "gpt-5") || strings.Contains(low, "gpt-4.1")) && !strings.Contains(low, "mini") && !strings.Contains(low, "nano") {
		score += 118
	}
	if hasToken(low, "pro") {
		score += 110
	}
	if strings.Contains(low, "deepseek") {
		score += 100
	}
	if strings.Contains(low, "qwen") {
		score += 85
	}

	params := extractParamBillions(low)
	switch {
	case params >= 70:
		score += 105
	case params >= 30:
		score += 90
	case params >= 14:
		score += 70
	case params >= 7:
		score += 45
	case params > 0 && params <= 4:
		score -= 35
	}

	// Penalize small/triage-oriented variants so flagship or coding models win.
	if strings.Contains(low, "mini") || strings.Contains(low, "nano") || strings.Contains(low, "haiku") {
		score -= 55
	}
	if strings.Contains(low, "flash") {
		score -= 45
	}
	if strings.Contains(low, "lite") {
		score -= 30
	}
	if strings.Contains(low, "latest") {
		score += 2
	}

	return score
}
