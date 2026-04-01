package whatsapp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"strings"
	"time"

	"whatsapp-gpt-bot/cache"
	"whatsapp-gpt-bot/types"

	"github.com/rs/zerolog/log"
)

const (
	GEMINI_EMBEDDING_API_URL = "https://generativelanguage.googleapis.com/v1beta/models/embedding-001:embedContent?key="
)

type Document struct {
	Text      string
	Embedding []float32
	Type      string    // "personality", "test_result", etc.
	CreatedAt time.Time // When the document was added
}

type VectorStore struct {
	documents       map[string]Document
	UserPersonality map[string]string // Maps user IDs to their personality profiles
	queryCache      *cache.Cache
}

func NewVectorStore() (*VectorStore, error) {
	vs := &VectorStore{
		documents:       make(map[string]Document),
		UserPersonality: make(map[string]string),
		queryCache:      cache.NewCache(1000),
	}

	// Load and process core soul document (harmonized identity and personality)
	if err := vs.loadDocument("soul.md", "personality"); err != nil {
		log.Debug().Err(err).Msg("Failed to load soul.md")
	}

	// Load and process default personality document
	if err := vs.loadDocument("personality.md", "personality"); err != nil {
		log.Debug().Err(err).Msg("Failed to load personality.md")
	}

	// Load bot's own identity
	if err := vs.loadDocument("identity.md", "identity"); err != nil {
		log.Debug().Err(err).Msg("Failed to load identity.md")
	}

	// Try to load additional personality documents if they exist
	personalityFiles := []string{"mbti_profiles.md", "enneagram_profiles.md"}
	for _, file := range personalityFiles {
		// Don't return error if these optional files don't exist
		_ = vs.loadDocument(file, "personality_type")
	}

	return vs, nil
}

func (vs *VectorStore) loadDocument(filename string, docType string) error {
	// Check cache
	cacheFile := filename + ".embedding.json"
	if doc, err := vs.loadFromCache(filename, cacheFile); err == nil {
		vs.documents[filename] = *doc
		return nil
	}

	content, err := os.ReadFile(filename)
	if err != nil {
		return fmt.Errorf("failed to read document %s: %w", filename, err)
	}

	text := string(content)

	embedding, err := embedDocument(text)
	if err != nil {
		// Don't fail startup just because embeddings aren't available (quota/access issues).
		// Log the embedding error and store the document text with an empty embedding so
		// the rest of the system can still use the text-only context.
		log.Debug().Err(err).Str("file", filename).Msg("failed to embed document; storing text-only")
		doc := Document{
			Text:      text,
			Embedding: []float32{},
			Type:      docType,
			CreatedAt: time.Now(),
		}
		vs.documents[filename] = doc
		// Do not attempt to cache the failed embedding
		return nil
	}

	doc := Document{
		Text:      text,
		Embedding: embedding,
		Type:      docType,
		CreatedAt: time.Now(),
	}

	vs.documents[filename] = doc

	// Save to cache
	if err := vs.saveToCache(cacheFile, &doc); err != nil {
		fmt.Printf("Warning: failed to save embedding cache: %v\n", err)
	}

	return nil
}

func (vs *VectorStore) loadFromCache(sourceFile, cacheFile string) (*Document, error) {
	sourceInfo, err := os.Stat(sourceFile)
	if err != nil {
		return nil, err
	}
	cacheInfo, err := os.Stat(cacheFile)
	if err != nil {
		return nil, err
	}

	if sourceInfo.ModTime().After(cacheInfo.ModTime()) {
		return nil, fmt.Errorf("cache is stale")
	}

	content, err := os.ReadFile(cacheFile)
	if err != nil {
		return nil, err
	}

	var doc Document
	if err := json.Unmarshal(content, &doc); err != nil {
		return nil, err
	}

	return &doc, nil
}

func (vs *VectorStore) saveToCache(cacheFile string, doc *Document) error {
	data, err := json.Marshal(doc)
	if err != nil {
		return err
	}
	return os.WriteFile(cacheFile, data, 0644)
}

type GeminiEmbeddingRequest struct {
	Model   string `json:"model"`
	Content struct {
		Parts []struct {
			Text string `json:"text"`
		} `json:"parts"`
	} `json:"content"`
}

type GeminiEmbeddingResponse struct {
	Embedding struct {
		Values []float32 `json:"values"`
	} `json:"embedding"`
}

// embedDocument sends text to an embedding API and returns its embedding vector.
// Supports LM Studio (default) and Gemini (fallback)
func embedDocument(text string) ([]float32, error) {
	// Check which embedding provider to use
	embeddingProvider := os.Getenv("EMBEDDING_PROVIDER")

	// If not set, default to LM Studio (since it supports both text generation and embeddings)
	if embeddingProvider == "" {
		embeddingProvider = "lmstudio"
	}

	switch strings.ToLower(embeddingProvider) {
	case "lmstudio", "lm-studio", "local":
		return embedWithLMStudio(text)
	case "gemini":
		return embedWithGemini(text)
	default:
		return embedWithLMStudio(text) // Default to LM Studio
	}
}

// embedWithLMStudio uses LM Studio's OpenAI-compatible API for embeddings
// LM Studio supports both text generation (Gemma 3) and embeddings (nomic-embed-text) simultaneously
func embedWithLMStudio(text string) ([]float32, error) {
	lmStudioURL := os.Getenv("LM_STUDIO_EMBEDDING_URL")
	if lmStudioURL == "" {
		// Try to use AI_ENDPOINT but change /chat/completions to /embeddings
		aiEndpoint := os.Getenv("AI_ENDPOINT")
		if aiEndpoint != "" {
			lmStudioURL = strings.Replace(aiEndpoint, "/chat/completions", "/embeddings", 1)
			// If no /chat/completions found, try appending /embeddings
			if lmStudioURL == aiEndpoint {
				lmStudioURL = strings.TrimSuffix(aiEndpoint, "/") + "/embeddings"
			}
		} else {
			lmStudioURL = "http://localhost:1234/v1/embeddings"
		}
	}

	model := os.Getenv("LM_STUDIO_EMBEDDING_MODEL")
	if model == "" {
		model = os.Getenv("MODEL_NAME") // Fallback to main model name
	}
	if model == "" {
		model = "nomic-embed-text" // Default
	}

	reqBody := map[string]interface{}{
		"model": model,
		"input": text,
	}

	jsonData, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal LM Studio request: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "POST", lmStudioURL, strings.NewReader(string(jsonData)))
	if err != nil {
		return nil, fmt.Errorf("failed to create LM Studio request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	if k := os.Getenv("OPENAI_API_KEY"); k != "" {
		req.Header.Set("Authorization", "Bearer "+k)
	} else if k := os.Getenv("AI_API_KEY"); k != "" {
		req.Header.Set("Authorization", "Bearer "+k)
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("failed to call LM Studio: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("failed to read LM Studio response: %w", err)
	}

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("LM Studio embedding failed with status %d: %s", resp.StatusCode, string(body))
	}

	// OpenAI-compatible embedding response format
	var lmStudioResp struct {
		Data []struct {
			Embedding []float64 `json:"embedding"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &lmStudioResp); err != nil {
		return nil, fmt.Errorf("failed to unmarshal LM Studio response: %w; body: %s", err, string(body))
	}

	if len(lmStudioResp.Data) == 0 || len(lmStudioResp.Data[0].Embedding) == 0 {
		return nil, fmt.Errorf("LM Studio returned empty embedding. Make sure nomic-embed-text model is loaded in LM Studio.")
	}

	// Convert []float64 to []float32
	embedding := make([]float32, len(lmStudioResp.Data[0].Embedding))
	for i, v := range lmStudioResp.Data[0].Embedding {
		embedding[i] = float32(v)
	}

	return embedding, nil
}

// embedWithGemini uses Gemini API (fallback option)
func embedWithGemini(text string) ([]float32, error) {
	reqBody := GeminiEmbeddingRequest{
		Model: "models/embedding-001",
		Content: struct {
			Parts []struct {
				Text string `json:"text"`
			} `json:"parts"`
		}{
			Parts: []struct {
				Text string `json:"text"`
			}{
				{Text: text},
			},
		},
	}

	jsonData, err := json.Marshal(reqBody)
	if err != nil {
		return nil, err
	}

	geminiAPIKey := os.Getenv(types.GEMINI_API_KEY_ENV)
	if geminiAPIKey == "" {
		return nil, fmt.Errorf("GEMINI_API_KEY environment variable not set")
	}

	maxRetries := 2
	var lastErr error
	for attempt := 0; attempt <= maxRetries; attempt++ {
		req, err := http.NewRequestWithContext(context.Background(), "POST", GEMINI_EMBEDDING_API_URL+geminiAPIKey, strings.NewReader(string(jsonData)))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")

		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			lastErr = err
			backoff := time.Duration(1<<attempt) * time.Second
			time.Sleep(backoff)
			continue
		}

		body, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			lastErr = fmt.Errorf("failed to read response body: %w", err)
			backoff := time.Duration(1<<attempt) * time.Second
			time.Sleep(backoff)
			continue
		}

		if resp.StatusCode != http.StatusOK {
			if resp.StatusCode == http.StatusTooManyRequests && attempt < maxRetries {
				if d, ok := parseRetryDelay(body); ok {
					time.Sleep(d)
				} else {
					time.Sleep(time.Duration(1<<attempt) * time.Second)
				}
				lastErr = fmt.Errorf("embedding request failed with status %d: %s", resp.StatusCode, string(body))
				continue
			}
			return nil, fmt.Errorf("embedding request failed with status %d: %s", resp.StatusCode, string(body))
		}

		var embeddingResp GeminiEmbeddingResponse
		if err := json.Unmarshal(body, &embeddingResp); err != nil {
			return nil, fmt.Errorf("failed to unmarshal embedding response: %w; body: %s", err, string(body))
		}

		if len(embeddingResp.Embedding.Values) == 0 {
			return nil, fmt.Errorf("received an empty embedding from the API. Full response: %s", string(body))
		}

		return embeddingResp.Embedding.Values, nil
	}

	return nil, fmt.Errorf("embedding failed after retries: %w", lastErr)
}

// parseRetryDelay attempts to extract a retryDelay string from a Gemini error response body
// and returns a time.Duration and true if found and parsed successfully.
func parseRetryDelay(body []byte) (time.Duration, bool) {
	var parsed map[string]interface{}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return 0, false
	}

	errObj, ok := parsed["error"].(map[string]interface{})
	if !ok {
		return 0, false
	}
	details, ok := errObj["details"].([]interface{})
	if !ok {
		return 0, false
	}
	for _, d := range details {
		dm, ok := d.(map[string]interface{})
		if !ok {
			continue
		}
		// RetryInfo may be present; look for retryDelay field
		if ri, found := dm["retryDelay"]; found {
			if s, ok := ri.(string); ok {
				if dur, err := time.ParseDuration(s); err == nil {
					return dur, true
				}
			}
		}
		// Some responses place retry info under a nested object
		for _, v := range dm {
			if m2, ok := v.(map[string]interface{}); ok {
				if rd, found := m2["retryDelay"]; found {
					if s, ok := rd.(string); ok {
						if dur, err := time.ParseDuration(s); err == nil {
							return dur, true
						}
					}
				}
			}
		}
	}
	return 0, false
}

func (vs *VectorStore) retrieveContext(query string, userID string) (string, error) {
	var relevantContext strings.Builder

	// 1. Include user-specific personality if known
	if personalityProfile, exists := vs.UserPersonality[userID]; exists {
		relevantContext.WriteString("## Information about the User you are talking to:\n")
		relevantContext.WriteString("The user has this personality profile: " + personalityProfile + "\n\n")
	}

	// Check query cache first
	key := sha256.Sum256([]byte(query))
	cacheKey := hex.EncodeToString(key[:])
	
	var queryEmbedding []float32
	var err error

	if cached, ok := vs.queryCache.Get(cacheKey); ok {
		if emb, ok2 := cached.([]float32); ok2 && len(emb) > 0 {
			queryEmbedding = emb
		}
	}

	if queryEmbedding == nil {
		queryEmbedding, err = embedDocument(query)
		if err == nil && len(queryEmbedding) > 0 {
			// store a copy in cache
			copyEmb := make([]float32, len(queryEmbedding))
			copy(copyEmb, queryEmbedding)
			vs.queryCache.Set(cacheKey, copyEmb, 24*time.Hour)
		}
	}

	// 2. Find most relevant additional documents
	if queryEmbedding != nil {
		type match struct {
			text       string
			similarity float64
		}
		var matches []match

		for name, doc := range vs.documents {
			// Skip identity/personality docs as they are provided in system prompt
			if name == "identity.md" || name == "personality.md" {
				continue
			}
			if len(doc.Embedding) == 0 {
				continue
			}
			similarity := cosineSimilarity(queryEmbedding, doc.Embedding)
			if similarity > 0.7 { // Threshold for relevance
				matches = append(matches, match{doc.Text, similarity})
			}
		}

		// Sort or just pick top matches (simple version: just append them)
		if len(matches) > 0 {
			relevantContext.WriteString("## Relevant Background Information:\n")
			for _, m := range matches {
				relevantContext.WriteString(m.text)
				relevantContext.WriteString("\n\n")
			}
		}
	} else {
		// Fallback if embedding fails: include some generic relevant docs or just skip
		log.Debug().Err(err).Msg("failed to embed query; using keyword-based or no additional context")
	}

	return relevantContext.String(), nil
}

func cosineSimilarity(a, b []float32) float64 {
	var dotProduct float64
	var aMagnitude float64
	var bMagnitude float64

	for i := 0; i < len(a); i++ {
		dotProduct += float64(a[i] * b[i])
		aMagnitude += float64(a[i] * a[i])
		bMagnitude += float64(b[i] * b[i])
	}

	if aMagnitude == 0 || bMagnitude == 0 {
		return 0.0
	}

	return dotProduct / (math.Sqrt(aMagnitude) * math.Sqrt(bMagnitude))
}

// SaveUserPersonality saves a user's personality test results
func (vs *VectorStore) SaveUserPersonality(userID string, personalityProfile string) error {
	// Save the personality profile in memory
	vs.UserPersonality[userID] = personalityProfile

	// Create a document for this user's personality profile
	docKey := fmt.Sprintf("user_personality_%s", userID)
	embedding, err := embedDocument(personalityProfile)
	if err != nil {
		// Don't fail the operation if embeddings are not available; store text-only profile
		fmt.Printf("Warning: failed to embed user personality for %s: %v. Saving text-only profile.\n", userID, err)
		vs.documents[docKey] = Document{
			Text:      personalityProfile,
			Embedding: []float32{},
			Type:      "user_personality",
			CreatedAt: time.Now(),
		}
		return nil
	}

	vs.documents[docKey] = Document{
		Text:      personalityProfile,
		Embedding: embedding,
		Type:      "user_personality",
		CreatedAt: time.Now(),
	}

	return nil
}

// GetPersonalityTestTemplate returns a personality test template based on the test type
func (vs *VectorStore) GetPersonalityTestTemplate(testType string) string {
	switch testType {
	case "mbti":
		return `
MBTI Personality Test

Please answer the following questions with A or B:

1. When you're in a social situation, do you:
   A) Get energized by interacting with many people
   B) Prefer deeper conversations with fewer people

2. When making decisions, do you tend to:
   A) Rely on objective facts and logic
   B) Consider people's feelings and circumstances

3. When planning your day, do you prefer:
   A) Having a structured schedule
   B) Keeping your options open

4. When solving problems, do you:
   A) Trust proven methods and experiences
   B) Look for new, creative approaches
`
	case "enneagram":
		return `
Enneagram Personality Test

Rate how much you identify with each statement (1-5, where 5 is strongly identify):

1. I strive for perfection and notice what needs improvement.
2. I prioritize helping others and meeting their needs.
3. I focus on achievement and how others perceive my success.
4. I value authenticity and expressing my unique identity.
5. I seek knowledge and understanding of the world around me.
6. I am loyal and vigilant about potential problems.
7. I seek new experiences and maintain a positive outlook.
8. I take charge of situations and protect those close to me.
9. I avoid conflict and try to create harmony around me.
`
	default:
		return "I don't have a template for that personality test type. I can offer MBTI or Enneagram tests."
	}
}

// InterpretPersonalityTest interprets the results of a personality test
func (vs *VectorStore) InterpretPersonalityTest(testType string, answers string) string {
	switch testType {
	case "mbti":
		// Clean the input: remove spaces, dots, and numbers to find just the A/B sequence
		// But first, try the "1. A" format
		responses := strings.ToUpper(answers)
		answerMap := make(map[int]rune)
		
		// Method 1: Look for "1.A" style
		for i := 1; i <= 4; i++ {
			prefix := fmt.Sprintf("%d.", i)
			idx := strings.Index(responses, prefix)
			if idx != -1 {
				// Look for next A or B after this index
				for j := idx + len(prefix); j < len(responses); j++ {
					char := rune(responses[j])
					if char == 'A' || char == 'B' {
						answerMap[i] = char
						break
					}
				}
			}
		}

		// Method 2: If Method 1 found nothing, just grab the first 4 A/B characters found in the string
		if len(answerMap) < 4 {
			found := 0
			for _, char := range responses {
				if char == 'A' || char == 'B' {
					found++
					answerMap[found] = char
					if found == 4 {
						break
					}
				}
			}
		}

		// Initialize counts for each preference
		counts := make(map[rune]int)

		// Map answers to dichotomies and count
		if ans, ok := answerMap[1]; ok {
			switch ans {
			case 'A':
				counts['E']++
			case 'B':
				counts['I']++
			}
		}
		if ans, ok := answerMap[2]; ok {
			switch ans {
			case 'A':
				counts['T']++
			case 'B':
				counts['F']++
			}
		}
		if ans, ok := answerMap[3]; ok {
			switch ans {
			case 'A':
				counts['J']++
			case 'B':
				counts['P']++
			}
		}
		if ans, ok := answerMap[4]; ok {
			switch ans {
			case 'A':
				counts['S']++
			case 'B':
				counts['N']++
			}
		}

		// If no valid A/B answers were found at all, return empty string
		if len(counts) == 0 {
			return ""
		}

		// Determine type
		var personality strings.Builder
		if counts['E'] >= counts['I'] {
			personality.WriteString("E")
		} else {
			personality.WriteString("I")
		}

		// Note: The order of S/N and T/F in MBTI is usually S/N then T/F
		if counts['S'] > counts['N'] {
			personality.WriteString("S")
		} else {
			personality.WriteString("N")
		}

		if counts['T'] > counts['F'] {
			personality.WriteString("T")
		} else {
			personality.WriteString("F")
		}

		if counts['J'] > counts['P'] {
			personality.WriteString("J")
		} else {
			personality.WriteString("P")
		}

		return fmt.Sprintf("Based on your answers, your MBTI type is: %s", personality.String())

	case "enneagram":
		// Simple Enneagram interpretation logic
		responses := answers
		answerMap := make(map[int]int)

		// Method 1: Look for "1. score" style
		for i := 1; i <= 9; i++ {
			prefix := fmt.Sprintf("%d.", i)
			idx := strings.Index(responses, prefix)
			if idx != -1 {
				for j := idx + len(prefix); j < len(responses); j++ {
					char := responses[j]
					if char >= '1' && char <= '5' {
						answerMap[i] = int(char - '0')
						break
					}
				}
			}
		}

		// Method 2: Fallback to any digits found
		if len(answerMap) < 9 {
			found := 0
			for _, char := range responses {
				if char >= '1' && char <= '5' {
					found++
					answerMap[found] = int(char - '0')
					if found == 9 {
						break
					}
				}
			}
		}

		highestScore := 0
		highestType := 0
		for i := 1; i <= 9; i++ {
			if score, ok := answerMap[i]; ok {
				if score > highestScore {
					highestScore = score
					highestType = i
				}
			}
		}

		if highestType == 0 {
			return ""
		}

		return fmt.Sprintf("Based on your answers, your primary Enneagram type is: Type %d", highestType)

	default:
		return "I couldn't interpret that personality test type."
	}
}
