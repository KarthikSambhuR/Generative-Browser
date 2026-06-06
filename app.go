package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/wailsapp/wails/v2/pkg/runtime"
)

const openAIResponsesURL = "https://api.openai.com/v1/responses"
const suggestionModel = "gpt-5.4-nano"
const pageGenerationModel = "gpt-5.4-nano"
const maxSuggestions = 5
const cacheDirName = "cache"
const cacheVersion = "v11-search-modes"
const sourcedQueryPrefix = "__morph_mode:sourced__"
const creativeQueryPrefix = "__morph_mode:creative__"

// App struct
type App struct {
	ctx context.Context
}

type suggestion struct {
	ID          string `json:"id"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Kind        string `json:"kind"`
	Query       string `json:"query"`
}

type suggestionEvent struct {
	RequestID string       `json:"requestId"`
	TabID     int64        `json:"tabId"`
	Query     string       `json:"query"`
	Item      *suggestion  `json:"item,omitempty"`
	Items     []suggestion `json:"items,omitempty"`
	Message   string       `json:"message,omitempty"`
}

type pageEvent struct {
	RequestID string `json:"requestId"`
	TabID     int64  `json:"tabId"`
	Title     string `json:"title"`
	Chunk     string `json:"chunk,omitempty"`
	Spec      string `json:"spec,omitempty"`
	Message   string `json:"message,omitempty"`
}

type responsesRequest struct {
	Model           string           `json:"model"`
	Instructions    string           `json:"instructions"`
	Input           string           `json:"input"`
	MaxOutputTokens int              `json:"max_output_tokens"`
	Stream          bool             `json:"stream"`
	Reasoning       *reasoningConfig `json:"reasoning,omitempty"`
	Text            *textConfig      `json:"text,omitempty"`
	Store           bool             `json:"store,omitempty"`
}

type reasoningConfig struct {
	Effort  string `json:"effort"`
	Summary string `json:"summary,omitempty"`
}

type textConfig struct {
	Verbosity string      `json:"verbosity"`
	Format    *textFormat `json:"format,omitempty"`
}

type textFormat struct {
	Type string `json:"type"`
}

type responseStreamEvent struct {
	Type              string             `json:"type"`
	Delta             string             `json:"delta"`
	Text              string             `json:"text"`
	Item              *responseOutput    `json:"item"`
	Error             *responseError     `json:"error"`
	Response          *responseObject    `json:"response"`
	IncompleteDetails *incompleteDetails `json:"incomplete_details"`
}

type incompleteDetails struct {
	Reason string `json:"reason"`
}

type responseError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	Type    string `json:"type"`
}

type responseObject struct {
	Output []responseOutput `json:"output"`
}

type responseOutput struct {
	Content []responseContent `json:"content"`
}

type responseContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type webSource struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Snippet string `json:"snippet"`
	Image   string `json:"image,omitempty"`
}

// NewApp creates a new App application struct
func NewApp() *App {
	return &App{}
}

// startup is called when the app starts. The context is saved
// so we can call the runtime methods
func (a *App) startup(ctx context.Context) {
	a.ctx = ctx
}

// SearchSuggestions starts a streamed LLM request for app and website suggestions.
func (a *App) SearchSuggestions(query string, tabID int64, requestID string, generationMode string) (string, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return "", errors.New("query is required")
	}
	requestID = strings.TrimSpace(requestID)
	if requestID == "" {
		requestID = newRequestID()
	}

	if cached, ok := readSuggestionsCache(query); ok {
		fmt.Printf("[Morph] suggestions cache hit requestID=%s query=%q items=%d\n", requestID, query, len(cached))
		go a.emitCachedSuggestions(requestID, tabID, query, cached)
		return requestID, nil
	}

	var apiKey string
	if generationMode == "superfast" {
		apiKey = readEnvValue("CEREBRAS_API_KEY")
		if apiKey == "" {
			apiKey = readEnvValue("CEREBRUS_API_KEY")
		}
		if apiKey == "" {
			err := errors.New("CEREBRAS_API_KEY or CEREBRUS_API_KEY is missing; add it to .env or your shell environment")
			logRequestProblem(requestID, query, err)
			return "", err
		}
	} else {
		apiKey = readEnvValue("OPENAI_API_KEY")
		if apiKey == "" {
			err := errors.New("OPENAI_API_KEY is missing; add it to .env or your shell environment")
			logRequestProblem(requestID, query, err)
			return "", err
		}
	}

	go a.streamSuggestions(requestID, tabID, query, apiKey, generationMode)

	return requestID, nil
}

func (a *App) emitCachedSuggestions(requestID string, tabID int64, query string, items []suggestion) {
	a.emit("suggestions:started", suggestionEvent{RequestID: requestID, TabID: tabID, Query: query})
	for _, item := range ensureFiveSuggestions(query, items) {
		copied := item
		a.emit("suggestions:item", suggestionEvent{
			RequestID: requestID,
			TabID:     tabID,
			Query:     query,
			Item:      &copied,
		})
	}
	a.emit("suggestions:done", suggestionEvent{
		RequestID: requestID,
		TabID:     tabID,
		Query:     query,
		Items:     ensureFiveSuggestions(query, items),
	})
}

// GeneratePageStream streams a creative custom page JSON spec for a clicked result.
func (a *App) GeneratePageStream(title string, description string, query string, tabID int64, requestID string, generationMode string) (string, error) {
	title = strings.TrimSpace(title)
	description = strings.TrimSpace(description)
	query = strings.TrimSpace(query)
	mode, cleanQuery := parsePageMode(query)
	query = cleanQuery
	requestID = strings.TrimSpace(requestID)
	if requestID == "" {
		requestID = newRequestID()
	}
	if title == "" {
		title = query
	}
	if title == "" {
		return "", errors.New("title or query is required")
	}

	if cached, ok := readPageCache(title, description, mode+"\n"+query); ok {
		fmt.Printf("[Morph] page cache hit requestID=%s title=%q query=%q bytes=%d\n", requestID, title, query, len(cached))
		go func() {
			a.emitPage("page:started", pageEvent{RequestID: requestID, TabID: tabID, Title: title})
			a.emitPage("page:done", pageEvent{
				RequestID: requestID,
				TabID:     tabID,
				Title:     title,
				Spec:      cached,
			})
		}()
		return requestID, nil
	}

	var apiKey string
	if generationMode == "superfast" {
		apiKey = readEnvValue("CEREBRAS_API_KEY")
		if apiKey == "" {
			apiKey = readEnvValue("CEREBRUS_API_KEY")
		}
		if apiKey == "" {
			err := errors.New("CEREBRAS_API_KEY or CEREBRUS_API_KEY is missing; add it to .env or your shell environment")
			logRequestProblem(requestID, query, err)
			a.emitPage("page:done", pageEvent{
				RequestID: requestID,
				TabID:     tabID,
				Title:     title,
				Spec:      fallbackPageJSON(title, description, query),
			})
			return requestID, nil
		}
	} else {
		apiKey = readEnvValue("OPENAI_API_KEY")
		if apiKey == "" {
			err := errors.New("OPENAI_API_KEY is missing; add it to .env or your shell environment")
			logRequestProblem(requestID, query, err)
			a.emitPage("page:done", pageEvent{
				RequestID: requestID,
				TabID:     tabID,
				Title:     title,
				Spec:      fallbackPageJSON(title, description, query),
			})
			return requestID, nil
		}
	}

	go a.streamPage(requestID, tabID, title, description, query, mode, apiKey, generationMode)
	return requestID, nil
}

func parsePageMode(query string) (string, string) {
	query = strings.TrimSpace(query)
	switch {
	case strings.HasPrefix(query, sourcedQueryPrefix):
		return "sourced", strings.TrimSpace(strings.TrimPrefix(query, sourcedQueryPrefix))
	case strings.HasPrefix(query, creativeQueryPrefix):
		return "creative", strings.TrimSpace(strings.TrimPrefix(query, creativeQueryPrefix))
	default:
		return "creative", query
	}
}

func pageModeQuery(mode string, query string) string {
	if mode == "sourced" {
		return sourcedQueryPrefix + "\n" + query
	}
	return creativeQueryPrefix + "\n" + query
}

func (a *App) streamPage(requestID string, tabID int64, title string, description string, query string, mode string, apiKey string, generationMode string) {
	fmt.Printf("[Morph] page stream started requestID=%s tabID=%d mode=%s title=%q query=%q provider=openai model=%s generationMode=%s\n", requestID, tabID, mode, title, query, pageGenerationModel, generationMode)
	a.emitPage("page:started", pageEvent{RequestID: requestID, TabID: tabID, Title: title})

	sources := []webSource{}
	if mode == "sourced" && !isSimpleToolQuery(query+" "+title) {
		foundSources, err := webSearch(queryForWebSearch(title, description, query), 6)
		if err != nil {
			fmt.Printf("[Morph] web search failed requestID=%s query=%q error=%v\n", requestID, query, err)
			a.emitPage("page:chunk", pageEvent{RequestID: requestID, TabID: tabID, Title: title, Chunk: "[web] Search failed; generating without live sources.\n"})
		} else {
			sources = foundSources
			fmt.Printf("[Morph] web search completed requestID=%s query=%q sources=%d\n", requestID, query, len(sources))
			a.emitPage("page:chunk", pageEvent{RequestID: requestID, TabID: tabID, Title: title, Chunk: fmt.Sprintf("[web] Found %d source(s).\n", len(sources))})
		}
	}

	spec, err := a.generateFullPageFromSources(requestID, tabID, title, description, query, mode, apiKey, sources, generationMode)
	if err != nil {
		a.emitPageError(requestID, tabID, title, query, err)
		return
	}
	if mode == "sourced" {
		spec = ensureSourcesInSpec(spec, sources)
	}
	fmt.Printf("[Morph] page stream completed requestID=%s bytes=%d sources=%d\n", requestID, len(spec), len(sources))
	if err := writePageCache(title, description, mode+"\n"+query, spec); err != nil {
		fmt.Printf("[Morph] page cache write failed requestID=%s error=%v\n", requestID, err)
	}
	a.emitPage("page:done", pageEvent{
		RequestID: requestID,
		TabID:     tabID,
		Title:     title,
		Spec:      spec,
	})
}


func (a *App) generateFullPageFromSources(requestID string, tabID int64, title string, description string, query string, mode string, apiKey string, sources []webSource, generationMode string) (string, error) {
	if generationMode == "superfast" {
		return a.generateFullPageFromSourcesCerebras(requestID, tabID, title, description, query, mode, apiKey, sources)
	}

	body := responsesRequest{
		Model:           pageGenerationModel,
		Instructions:    fullPageInstructions(mode),
		Input:           pageGenerationBrief(title, description, query, mode, sources),
		MaxOutputTokens: 16000,
		Stream:          true,
		Reasoning: &reasoningConfig{
			Effort: "low",
		},
		Text: &textConfig{
			Verbosity: "medium",
			Format: &textFormat{
				Type: "json_object",
			},
		},
		Store: true,
	}

	payload, err := json.Marshal(body)
	if err != nil {
		return "", err
	}

	req, err := http.NewRequestWithContext(a.ctx, http.MethodPost, openAIResponsesURL, bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")

	client := &http.Client{Timeout: 150 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		responseBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", fmt.Errorf("OpenAI page stream returned %s: %s", resp.Status, strings.TrimSpace(string(responseBody)))
	}

	content := strings.Builder{}
	seenEventTypes := map[string]int{}
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 4096), 8*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" {
			continue
		}
		if data == "[DONE]" {
			break
		}

		var event responseStreamEvent
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			fmt.Printf("[Morph] ignored malformed page stream event requestID=%s error=%v body=%s\n", requestID, err, truncateForLog(data, 500))
			continue
		}
		if event.Type != "" {
			seenEventTypes[event.Type]++
		}
		if event.Type == "response.incomplete" {
			fmt.Printf("[Morph] OpenAI page response incomplete requestID=%s reason=%s event=%s\n", requestID, incompleteReason(event), truncateForLog(data, 700))
		}
		if event.Error != nil {
			return "", fmt.Errorf("OpenAI page stream error type=%s code=%s message=%s", event.Error.Type, event.Error.Code, event.Error.Message)
		}

		textDelta := textFromStreamEvent(event)
		if textDelta == "" {
			continue
		}
		content.WriteString(textDelta)
		a.emitPage("page:chunk", pageEvent{
			RequestID: requestID,
			TabID:     tabID,
			Title:     title,
			Chunk:     textDelta,
		})
	}
	if err := scanner.Err(); err != nil {
		return "", err
	}
	spec := extractJSONObject(content.String())
	if spec == "" {
		fmt.Printf("[Morph] page stream returned no JSON requestID=%s eventTypes=%v raw=%s\n", requestID, seenEventTypes, truncateForLog(content.String(), 1200))
		return "", fmt.Errorf("page generation returned no JSON")
	}
	return spec, nil
}

type cerebrasMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type cerebrasRequest struct {
	Model       string            `json:"model"`
	Messages    []cerebrasMessage `json:"messages"`
	Stream      bool              `json:"stream"`
	MaxTokens   int               `json:"max_tokens,omitempty"`
	Temperature float64           `json:"temperature,omitempty"`
	TopP        float64           `json:"top_p,omitempty"`
}

type cerebrasStreamResponse struct {
	Choices []struct {
		Delta struct {
			Content string `json:"content"`
		} `json:"delta"`
	} `json:"choices"`
}

func (a *App) generateFullPageFromSourcesCerebras(requestID string, tabID int64, title string, description string, query string, mode string, apiKey string, sources []webSource) (string, error) {
	maxRetries := 10
	var lastErr error

	for attempt := 1; attempt <= maxRetries; attempt++ {
		if attempt > 1 {
			fmt.Printf("[Morph] Retrying generateFullPageFromSourcesCerebras, attempt %d/%d...\n", attempt, maxRetries)
			// Emit page:started to clear/reset the stream state in the frontend
			a.emitPage("page:started", pageEvent{RequestID: requestID, TabID: tabID, Title: title})
			time.Sleep(1 * time.Second)
		}

		spec, err := a.tryGenerateFullPageFromSourcesCerebras(requestID, tabID, title, description, query, mode, apiKey, sources)
		if err == nil && spec != "" {
			return spec, nil
		}
		if err != nil {
			lastErr = err
		} else {
			lastErr = fmt.Errorf("empty page spec generated")
		}
	}

	return "", fmt.Errorf("failed page generation after %d attempts: %v", maxRetries, lastErr)
}

func (a *App) tryGenerateFullPageFromSourcesCerebras(requestID string, tabID int64, title string, description string, query string, mode string, apiKey string, sources []webSource) (string, error) {
	fmt.Printf("[Morph] tryGenerateFullPageFromSourcesCerebras started requestID=%s tabID=%d mode=%s title=%q query=%q\n", requestID, tabID, mode, title, query)
	systemPrompt := fullPageInstructions(mode)
	input := pageGenerationBrief(title, description, query, mode, sources)

	body := cerebrasRequest{
		Model: "zai-glm-4.7",
		Messages: []cerebrasMessage{
			{Role: "system", Content: systemPrompt},
			{Role: "user", Content: input},
		},
		Stream:      true,
		MaxTokens:   65000,
		Temperature: 1,
		TopP:        0.95,
	}

	payload, err := json.Marshal(body)
	if err != nil {
		return "", err
	}

	apiURL := readEnvValue("CEREBRAS_API_URL")
	if apiURL == "" {
		apiURL = readEnvValue("CEREBRUS_API_URL")
	}
	if apiURL == "" {
		apiURL = "https://api.cerebras.ai/v1/chat/completions"
	}

	req, err := http.NewRequestWithContext(a.ctx, http.MethodPost, apiURL, bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer " + apiKey)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 150 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		responseBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return "", fmt.Errorf("Cerebras page stream returned %s: %s", resp.Status, strings.TrimSpace(string(responseBody)))
	}

	content := strings.Builder{}
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 4096), 8*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if line == "data: [DONE]" {
			break
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		var event cerebrasStreamResponse
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			continue
		}
		if len(event.Choices) > 0 {
			textDelta := event.Choices[0].Delta.Content
			if textDelta != "" {
				content.WriteString(textDelta)
				a.emitPage("page:chunk", pageEvent{
					RequestID: requestID,
					TabID:     tabID,
					Title:     title,
					Chunk:     textDelta,
				})
			}
		}
	}

	if err := scanner.Err(); err != nil {
		return "", err
	}
	spec := extractJSONObject(content.String())
	if spec == "" {
		fmt.Printf("[Morph] Cerebras page stream returned no JSON requestID=%s raw=%s\n", requestID, truncateForLog(content.String(), 1200))
		return "", fmt.Errorf("page generation returned no JSON")
	}
	return spec, nil
}

func pageGenerationBrief(title string, description string, query string, mode string, sources []webSource) string {
	sourceJSON, err := json.MarshalIndent(sources, "", "  ")
	if err != nil {
		sourceJSON = []byte("[]")
	}
	return fmt.Sprintf(`Build this Morph generated website/app.

Title: %s
Description: %s
Original query: %s
Search mode: %s
Web sources JSON: %s

Rules:
- Return one valid JSON object only. No markdown, no code fences, no prose outside JSON.
- Schema: {"title":"string","subtitle":"string","mode":"mini_app|article|dashboard|tool|game|gallery","sourceUrl":"string","customHtml":"string","customCss":"string","customJs":"string","sections":[]}
- All strings in the JSON, including "customHtml" and "customCss", MUST be properly escaped. Newlines inside HTML or CSS strings must be written as '\n', not as literal unescaped newlines.
- customHtml is a body fragment only. Do not include html/head/body/style/script tags.
- customCss is complete CSS. Use @import for Google Fonts when useful.
- In sourced mode: use no JavaScript unless the query is a simple working tool like calculator, todo, notes, timer, converter, or checklist. For normal informational sourced searches, customJs should be an empty string.
- In sourced mode: Enforce a bold, highly informational, and modern premium design. Use vibrant accents, elegant typography, clean grid structures, and clear content blocks. Make it feel like a fully realized, actual website where users can get rich, detailed information. Do not use plain text blocks or empty screens.
- In creative mode: JavaScript is allowed when it makes the generated page more alive or functional.
- If sourced mode gets an informational/real topic, use the web sources as the factual basis and cite them visibly and beautifully in the page.
- Include a polished Sources section in customHtml with links to the provided source URLs when sources are provided.
- If a source has an image URL, use it as a visual asset with alt text and nearby attribution.
- Use lucide-style inline SVG icons where icons help. Do not load icon libraries.
- Use modern design standards: responsive layout, strong typography, accessible contrast, stable spacing, polished controls, and no browser-default-looking UI.
- If creative mode receives an app/tool/game request, generate the full functional UI from this single request.
- Match the user's intent directly first; add creativity only after the direct result is solved.`, title, description, query, mode, string(sourceJSON))
}

func fullPageInstructions(mode string) string {
	if mode == "sourced" {
		return "You are Morph's sourced-search page generator. Create one accurate, beautifully structured, bold, and informational HTML/CSS website from provided web sources. Enforce a premium, modern design with rich typography, clean grids, and prominent information display. Cite sources and factual information clearly. Return only one JSON object."
	}
	return "You are Morph's creative-search page generator. Create one coherent, imaginative, bold, and polished generated page as JSON containing customHtml, customCss, and customJs. Return only one JSON object."
}

func queryForWebSearch(title string, description string, query string) string {
	for _, value := range []string{query, title, description} {
		value = strings.TrimSpace(value)
		if value != "" {
			return value
		}
	}
	return title
}

func isSimpleToolQuery(value string) bool {
	value = strings.ToLower(value)
	simpleTools := []string{
		"calculator", "calc", "todo", "to-do", "task list", "notes", "notepad",
		"timer", "stopwatch", "countdown", "pomodoro", "converter", "checklist",
		"unit converter", "currency converter", "password generator", "qr code",
	}
	for _, tool := range simpleTools {
		if strings.Contains(value, tool) {
			return true
		}
	}
	return false
}

func webSearch(query string, limit int) ([]webSource, error) {
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, nil
	}
	searchURL := "https://html.duckduckgo.com/html/?q=" + url.QueryEscape(query)
	req, err := http.NewRequest(http.MethodGet, searchURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 Morph/1.0")

	client := &http.Client{Timeout: 12 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, fmt.Errorf("search returned %s", resp.Status)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 512*1024))
	if err != nil {
		return nil, err
	}
	sources := parseDuckDuckGoResults(string(body), limit)
	enrichSourceImages(client, sources)
	return sources, nil
}

func parseDuckDuckGoResults(body string, limit int) []webSource {
	linkRe := regexp.MustCompile(`(?is)<a[^>]+class="[^"]*result__a[^"]*"[^>]+href="([^"]+)"[^>]*>(.*?)</a>`)
	snippetRe := regexp.MustCompile(`(?is)<a[^>]+class="[^"]*result__snippet[^"]*"[^>]*>(.*?)</a>|<div[^>]+class="[^"]*result__snippet[^"]*"[^>]*>(.*?)</div>`)
	linkMatches := linkRe.FindAllStringSubmatchIndex(body, -1)
	snippetMatches := snippetRe.FindAllStringSubmatch(body, -1)
	snippets := make([]string, 0, len(snippetMatches))
	for _, match := range snippetMatches {
		text := ""
		if len(match) > 1 && match[1] != "" {
			text = match[1]
		} else if len(match) > 2 {
			text = match[2]
		}
		snippets = append(snippets, cleanHTMLText(text))
	}

	results := []webSource{}
	seen := map[string]bool{}
	for index, match := range linkMatches {
		if len(results) >= limit {
			break
		}
		rawHref := body[match[2]:match[3]]
		rawTitle := body[match[4]:match[5]]
		resolved := resolveDuckDuckGoURL(cleanAttr(rawHref))
		if resolved == "" || seen[resolved] {
			continue
		}
		seen[resolved] = true
		snippet := ""
		if index < len(snippets) {
			snippet = snippets[index]
		}
		results = append(results, webSource{
			Title:   cleanHTMLText(rawTitle),
			URL:     resolved,
			Snippet: snippet,
		})
	}
	return results
}

func enrichSourceImages(client *http.Client, sources []webSource) {
	for index := range sources {
		if index >= 4 {
			return
		}
		image := fetchSourceImage(client, sources[index].URL)
		if image != "" {
			sources[index].Image = image
		}
	}
}

func fetchSourceImage(client *http.Client, sourceURL string) string {
	req, err := http.NewRequest(http.MethodGet, sourceURL, nil)
	if err != nil {
		return ""
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 Morph/1.0")
	resp, err := client.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return ""
	}
	bodyBytes, err := io.ReadAll(io.LimitReader(resp.Body, 256*1024))
	if err != nil {
		return ""
	}
	body := string(bodyBytes)
	patterns := []*regexp.Regexp{
		regexp.MustCompile(`(?is)<meta[^>]+property=["']og:image["'][^>]+content=["']([^"']+)["']`),
		regexp.MustCompile(`(?is)<meta[^>]+content=["']([^"']+)["'][^>]+property=["']og:image["']`),
		regexp.MustCompile(`(?is)<meta[^>]+name=["']twitter:image["'][^>]+content=["']([^"']+)["']`),
		regexp.MustCompile(`(?is)<meta[^>]+content=["']([^"']+)["'][^>]+name=["']twitter:image["']`),
	}
	for _, pattern := range patterns {
		match := pattern.FindStringSubmatch(body)
		if len(match) < 2 {
			continue
		}
		image := cleanAttr(match[1])
		if image == "" {
			continue
		}
		if absolute, ok := absoluteURL(sourceURL, image); ok {
			return absolute
		}
	}
	return ""
}

func resolveDuckDuckGoURL(rawHref string) string {
	rawHref = html.UnescapeString(strings.TrimSpace(rawHref))
	parsed, err := url.Parse(rawHref)
	if err == nil {
		if uddg := parsed.Query().Get("uddg"); uddg != "" {
			if decoded, decodeErr := url.QueryUnescape(uddg); decodeErr == nil {
				return decoded
			}
			return uddg
		}
		if parsed.Scheme == "http" || parsed.Scheme == "https" {
			return parsed.String()
		}
	}
	return ""
}

func absoluteURL(baseURL string, value string) (string, bool) {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil {
		return "", false
	}
	if parsed.Scheme == "http" || parsed.Scheme == "https" {
		return parsed.String(), true
	}
	base, err := url.Parse(baseURL)
	if err != nil {
		return "", false
	}
	return base.ResolveReference(parsed).String(), true
}

func cleanHTMLText(value string) string {
	tagRe := regexp.MustCompile(`(?is)<[^>]+>`)
	value = tagRe.ReplaceAllString(value, " ")
	value = html.UnescapeString(value)
	return strings.Join(strings.Fields(value), " ")
}

func cleanAttr(value string) string {
	return html.UnescapeString(strings.TrimSpace(value))
}

func ensureSourcesInSpec(spec string, sources []webSource) string {
	if len(sources) == 0 {
		return spec
	}
	var page map[string]interface{}
	if err := json.Unmarshal([]byte(spec), &page); err != nil {
		return spec
	}
	customHTML, _ := page["customHtml"].(string)
	if strings.Contains(strings.ToLower(customHTML), "morph-source-list") {
		return spec
	}
	customCSS, _ := page["customCss"].(string)
	page["customHtml"] = customHTML + sourceSectionHTML(sources)
	page["customCss"] = customCSS + sourceSectionCSS()
	if page["sourceUrl"] == nil || strings.TrimSpace(fmt.Sprint(page["sourceUrl"])) == "" {
		page["sourceUrl"] = sources[0].URL
	}
	bytes, err := json.Marshal(page)
	if err != nil {
		return spec
	}
	return string(bytes)
}

func sourceSectionHTML(sources []webSource) string {
	var builder strings.Builder
	builder.WriteString(`<section class="morph-source-list" aria-label="Sources"><h2>Sources</h2><div class="morph-source-grid">`)
	for _, source := range sources {
		builder.WriteString(`<a class="morph-source-card" href="`)
		builder.WriteString(htmlText(source.URL, "#"))
		builder.WriteString(`" target="_blank" rel="noreferrer">`)
		if source.Image != "" {
			builder.WriteString(`<img src="`)
			builder.WriteString(htmlText(source.Image, ""))
			builder.WriteString(`" alt="">`)
		}
		builder.WriteString(`<strong>`)
		builder.WriteString(htmlText(source.Title, source.URL))
		builder.WriteString(`</strong><span>`)
		builder.WriteString(htmlText(source.Snippet, source.URL))
		builder.WriteString(`</span></a>`)
	}
	builder.WriteString(`</div></section>`)
	return builder.String()
}

func sourceSectionCSS() string {
	return `.morph-source-list{margin:32px auto 0;max-width:1100px;padding:24px;border-radius:22px;background:rgba(255,255,255,.08);border:1px solid rgba(255,255,255,.14);box-sizing:border-box}.morph-source-list h2{margin:0 0 16px;font-size:clamp(22px,3vw,34px)}.morph-source-grid{display:grid;grid-template-columns:repeat(auto-fit,minmax(220px,1fr));gap:14px}.morph-source-card{display:grid;gap:8px;text-decoration:none;color:inherit;padding:14px;border-radius:16px;background:rgba(255,255,255,.1);border:1px solid rgba(255,255,255,.12);overflow:hidden}.morph-source-card:hover{transform:translateY(-1px)}.morph-source-card img{width:100%;aspect-ratio:16/9;object-fit:cover;border-radius:12px}.morph-source-card strong{font-size:15px}.morph-source-card span{font-size:13px;line-height:1.45;opacity:.78}`
}

func assemblePageSpec(title string, description string, query string, html string, css string, js string) string {
	spec := map[string]interface{}{
		"title":      title,
		"subtitle":   description,
		"sourceUrl":  "https://apps.morph.local/" + slugForCache(title+" "+query),
		"mode":       pageModeForQuery(title + " " + query),
		"customHtml": html,
		"customCss":  css,
		"customJs":   js,
		"sections":   []map[string]interface{}{},
	}
	bytes, err := json.Marshal(spec)
	if err != nil {
		return fallbackPageJSON(title, description, query)
	}
	return string(bytes)
}

func htmlText(value string, fallback string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		value = fallback
	}
	replacer := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&#39;")
	return replacer.Replace(value)
}

func pageModeForQuery(value string) string {
	lower := strings.ToLower(value)
	switch {
	case strings.Contains(lower, "game"):
		return "game"
	case strings.Contains(lower, "gallery"):
		return "gallery"
	case strings.Contains(lower, "dashboard"):
		return "dashboard"
	case strings.Contains(lower, "calculator") || strings.Contains(lower, "todo") || strings.Contains(lower, "notes") || strings.Contains(lower, "tool"):
		return "tool"
	default:
		return "mini_app"
	}
}

func stripCodeFences(value string) string {
	value = strings.TrimSpace(value)
	if !strings.HasPrefix(value, "```") {
		return value
	}
	lines := strings.Split(value, "\n")
	if len(lines) <= 2 {
		return value
	}
	if strings.HasPrefix(strings.TrimSpace(lines[0]), "```") {
		lines = lines[1:]
	}
	if len(lines) > 0 && strings.HasPrefix(strings.TrimSpace(lines[len(lines)-1]), "```") {
		lines = lines[:len(lines)-1]
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

func slugForCache(value string) string {
	value = normalizeCacheKey(value)
	var builder strings.Builder
	lastDash := false
	for _, char := range value {
		if (char >= 'a' && char <= 'z') || (char >= '0' && char <= '9') {
			builder.WriteRune(char)
			lastDash = false
			continue
		}
		if !lastDash {
			builder.WriteRune('-')
			lastDash = true
		}
	}
	slug := strings.Trim(builder.String(), "-")
	if slug == "" {
		return "generated-page"
	}
	return slug
}

func (a *App) emitPageError(requestID string, tabID int64, title string, query string, err error) {
	logRequestProblem(requestID, query, err)
	a.emitPage("page:error", pageEvent{
		RequestID: requestID,
		TabID:     tabID,
		Title:     title,
		Message:   err.Error(),
		Spec:      fallbackPageJSON(title, "A fallback generated interface.", query),
	})
}

// GeneratePage returns a JSON page spec that the frontend can render safely.
func (a *App) GeneratePage(title string, description string, query string, generationMode string) (string, error) {
	title = strings.TrimSpace(title)
	description = strings.TrimSpace(description)
	query = strings.TrimSpace(query)
	if title == "" {
		title = query
	}
	if title == "" {
		return "", errors.New("title or query is required")
	}

	apiKey := readEnvValue("OPENAI_API_KEY")
	if apiKey == "" {
		err := errors.New("OPENAI_API_KEY is missing; add it to .env or your shell environment")
		logRequestProblem("page", query, err)
		return fallbackPageJSON(title, description, query), nil
	}

	fmt.Printf("[Morph] page generation started title=%q query=%q provider=openai model=%s\n", title, query, pageGenerationModel)

	body := responsesRequest{
		Model:           pageGenerationModel,
		Instructions:    "You create compact JSON UI specs for Morph. Return only JSON. No markdown. The JSON must be safe and renderable with this schema: {\"title\":\"string\",\"subtitle\":\"string\",\"theme\":{\"accent\":\"#hex\",\"mood\":\"dark|clean|neon|calm\"},\"sections\":[{\"type\":\"hero|stats|cards|list|form|table|controls\",\"title\":\"string\",\"description\":\"string\",\"items\":[{\"title\":\"string\",\"description\":\"string\",\"value\":\"string\",\"label\":\"string\"}],\"fields\":[{\"label\":\"string\",\"placeholder\":\"string\"}],\"actions\":[{\"label\":\"string\",\"action\":\"increment|toggle|append|highlight\",\"target\":\"string\"}]}]}. Make the UI feel cool, functional, and specific to the title. Include 5 to 7 sections. Include at least one controls or form section and one stats/cards/list section.",
		Input:           fmt.Sprintf("Title: %s\nDescription: %s\nOriginal query: %s\nCreate the JSON UI page spec.", title, description, query),
		MaxOutputTokens: 5000,
		Stream:          false,
		Store:           true,
	}

	payload, err := json.Marshal(body)
	if err != nil {
		logRequestProblem("page", query, err)
		return fallbackPageJSON(title, description, query), nil
	}

	req, err := http.NewRequestWithContext(a.ctx, http.MethodPost, openAIResponsesURL, bytes.NewReader(payload))
	if err != nil {
		logRequestProblem("page", query, err)
		return fallbackPageJSON(title, description, query), nil
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 90 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		logRequestProblem("page", query, err)
		return fallbackPageJSON(title, description, query), nil
	}
	defer resp.Body.Close()

	responseBody, err := io.ReadAll(io.LimitReader(resp.Body, 1024*1024))
	if err != nil {
		logRequestProblem("page", query, err)
		return fallbackPageJSON(title, description, query), nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		err := fmt.Errorf("OpenAI page API returned %s: %s", resp.Status, truncateForLog(string(responseBody), 1200))
		logRequestProblem("page", query, err)
		return fallbackPageJSON(title, description, query), nil
	}

	var response responseObject
	if err := json.Unmarshal(responseBody, &response); err != nil {
		logRequestProblem("page", query, err)
		return fallbackPageJSON(title, description, query), nil
	}

	var text strings.Builder
	for _, output := range response.Output {
		text.WriteString(textFromOutput(output))
	}
	spec := extractJSONObject(text.String())
	if spec == "" {
		fmt.Printf("[Morph] page generation returned no JSON; using fallback raw=%s\n", truncateForLog(text.String(), 900))
		return fallbackPageJSON(title, description, query), nil
	}
	fmt.Printf("[Morph] page generation completed title=%q bytes=%d\n", title, len(spec))
	return spec, nil
}

func (a *App) streamSuggestionsCerebras(requestID string, tabID int64, query string, apiKey string) {
	maxRetries := 10
	var lastErr error

	for attempt := 1; attempt <= maxRetries; attempt++ {
		if attempt > 1 {
			fmt.Printf("[Morph] Retrying streamSuggestionsCerebras, attempt %d/%d...\n", attempt, maxRetries)
			// Emit suggestions:started to clear/reset suggestions state in frontend
			a.emit("suggestions:started", suggestionEvent{
				RequestID: requestID,
				TabID:     tabID,
				Query:     query,
			})
			time.Sleep(1 * time.Second)
		}

		err := a.tryStreamSuggestionsCerebras(requestID, tabID, query, apiKey)
		if err == nil {
			return
		}
		lastErr = err
	}

	a.emitSuggestionError(requestID, tabID, query, fmt.Errorf("failed suggestions after %d attempts: %v", maxRetries, lastErr))
}

func (a *App) tryStreamSuggestionsCerebras(requestID string, tabID int64, query string, apiKey string) error {
	fmt.Printf("[Morph] tryStreamSuggestionsCerebras started requestID=%s tabID=%d query=%q\n", requestID, tabID, query)

	body := cerebrasRequest{
		Model: "zai-glm-4.7",
		Messages: []cerebrasMessage{
			{Role: "system", Content: "You are Morph's Creative Search suggestion engine. Return only JSON. No markdown. No prose. Shape: {\"suggestions\":[{\"id\":\"short-kebab-id\",\"title\":\"Creative app or website title\",\"description\":\"One vivid short sentence about what it opens or creates.\",\"kind\":\"website|generated_app|tool\",\"query\":\"actionable query to generate\"}]}. Return exactly 5 suggestions in one JSON object. Every result can be imaginative. Do not rank by practicality. Keep each result connected to the user's search, but give each one a distinct creative angle, interface idea, or generated experience."},
			{Role: "user", Content: fmt.Sprintf("User searched in Creative Search: %q. Suggest 5 creative generated app or website results.", query)},
		},
		Stream:      true,
		MaxTokens:   65000,
		Temperature: 1,
		TopP:        0.95,
	}

	payload, err := json.Marshal(body)
	if err != nil {
		return err
	}

	apiURL := readEnvValue("CEREBRAS_API_URL")
	if apiURL == "" {
		apiURL = readEnvValue("CEREBRUS_API_URL")
	}
	if apiURL == "" {
		apiURL = "https://api.cerebras.ai/v1/chat/completions"
	}

	req, err := http.NewRequestWithContext(a.ctx, http.MethodPost, apiURL, bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer " + apiKey)
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 90 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		responseBody, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		return fmt.Errorf("Cerebras API returned %s: %s", resp.Status, strings.TrimSpace(string(responseBody)))
	}

	content := strings.Builder{}
	emitted := map[string]bool{}
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 4096), 8*1024*1024)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		if line == "data: [DONE]" {
			break
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		var event cerebrasStreamResponse
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			continue
		}
		if len(event.Choices) > 0 {
			textDelta := event.Choices[0].Delta.Content
			if textDelta != "" {
				content.WriteString(textDelta)
				for _, item := range extractSuggestions(content.String()) {
					if len(emitted) >= maxSuggestions {
						break
					}
					key := item.ID
					if key == "" {
						key = strings.ToLower(item.Title)
					}
					if key == "" || emitted[key] {
						continue
					}
					emitted[key] = true
					copied := item
					a.emit("suggestions:item", suggestionEvent{
						RequestID: requestID,
						TabID:     tabID,
						Query:     query,
						Item:      &copied,
					})
				}
			}
		}
	}

	if err := scanner.Err(); err != nil {
		return err
	}

	items := extractSuggestions(content.String())
	items = ensureFiveSuggestions(query, items)
	if err := writeSuggestionsCache(query, items); err != nil {
		fmt.Printf("[Morph] suggestions cache write failed requestID=%s error=%v\n", requestID, err)
	}
	for _, item := range items {
		key := suggestionKey(item)
		if key == "" || emitted[key] {
			continue
		}
		emitted[key] = true
		copied := item
		a.emit("suggestions:item", suggestionEvent{
			RequestID: requestID,
			TabID:     tabID,
			Query:     query,
			Item:      &copied,
		})
	}
	a.emit("suggestions:done", suggestionEvent{
		RequestID: requestID,
		TabID:     tabID,
		Query:     query,
		Items:     items,
	})
	return nil
}

func (a *App) streamSuggestions(requestID string, tabID int64, query string, apiKey string, generationMode string) {
	if generationMode == "superfast" {
		a.streamSuggestionsCerebras(requestID, tabID, query, apiKey)
		return
	}

	fmt.Printf("[Morph] suggestions request started requestID=%s tabID=%d query=%q provider=openai model=%s\n", requestID, tabID, query, suggestionModel)

	a.emit("suggestions:started", suggestionEvent{
		RequestID: requestID,
		TabID:     tabID,
		Query:     query,
	})

	body := responsesRequest{
		Model:           suggestionModel,
		Instructions:    "You are Morph's Creative Search suggestion engine. Return only JSON. No markdown. No prose. Shape: {\"suggestions\":[{\"id\":\"short-kebab-id\",\"title\":\"Creative app or website title\",\"description\":\"One vivid short sentence about what it opens or creates.\",\"kind\":\"website|generated_app|tool\",\"query\":\"actionable query to generate\"}]}. Return exactly 5 suggestions in one JSON object. Every result can be imaginative. Do not rank by practicality. Keep each result connected to the user's search, but give each one a distinct creative angle, interface idea, or generated experience.",
		Input:           fmt.Sprintf("User searched in Creative Search: %q. Suggest 5 creative generated app or website results.", query),
		MaxOutputTokens: 3500,
		Stream:          true,
		Reasoning: &reasoningConfig{
			Effort: "low",
		},
		Text: &textConfig{
			Verbosity: "low",
			Format: &textFormat{
				Type: "text",
			},
		},
	}

	payload, err := json.Marshal(body)
	if err != nil {
		logRequestProblem(requestID, query, err)
		a.emitSuggestionError(requestID, tabID, query, err)
		return
	}

	req, err := http.NewRequestWithContext(a.ctx, http.MethodPost, openAIResponsesURL, bytes.NewReader(payload))
	if err != nil {
		logRequestProblem(requestID, query, err)
		a.emitSuggestionError(requestID, tabID, query, err)
		return
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")

	client := &http.Client{Timeout: 90 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		logRequestProblem(requestID, query, err)
		a.emitSuggestionError(requestID, tabID, query, err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		responseBody, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
		err := fmt.Errorf("OpenAI API returned %s: %s", resp.Status, strings.TrimSpace(string(responseBody)))
		logRequestProblem(requestID, query, err)
		a.emitSuggestionError(requestID, tabID, query, err)
		return
	}

	content := strings.Builder{}
	emitted := map[string]bool{}
	seenEventTypes := map[string]int{}

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 1024), 1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}

		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" {
			continue
		}
		if data == "[DONE]" {
			break
		}

		var event responseStreamEvent
		if err := json.Unmarshal([]byte(data), &event); err != nil {
			fmt.Printf("[Morph] ignored malformed stream event requestID=%s error=%v body=%s\n", requestID, err, truncateForLog(data, 500))
			continue
		}
		if event.Type != "" {
			seenEventTypes[event.Type]++
		}
		if event.Type == "response.incomplete" {
			fmt.Printf("[Morph] OpenAI response incomplete requestID=%s reason=%s event=%s\n", requestID, incompleteReason(event), truncateForLog(data, 700))
		}
		if event.Error != nil {
			err := fmt.Errorf("OpenAI stream error type=%s code=%s message=%s", event.Error.Type, event.Error.Code, event.Error.Message)
			logRequestProblem(requestID, query, err)
			a.emitSuggestionError(requestID, tabID, query, err)
			return
		}

		textDelta := textFromStreamEvent(event)
		if textDelta == "" {
			continue
		}
		content.WriteString(textDelta)

		for _, item := range extractSuggestions(content.String()) {
			if len(emitted) >= maxSuggestions {
				break
			}
			key := item.ID
			if key == "" {
				key = strings.ToLower(item.Title)
			}
			if key == "" || emitted[key] {
				continue
			}
			emitted[key] = true
			copied := item
			a.emit("suggestions:item", suggestionEvent{
				RequestID: requestID,
				TabID:     tabID,
				Query:     query,
				Item:      &copied,
			})
		}
	}

	if err := scanner.Err(); err != nil {
		logRequestProblem(requestID, query, err)
		a.emitSuggestionError(requestID, tabID, query, err)
		return
	}

	items := extractSuggestions(content.String())
	if len(items) < maxSuggestions {
		fmt.Printf("[Morph] suggestion stream produced %d parseable items; filling fallback requestID=%s eventTypes=%v raw=%s\n", len(items), requestID, seenEventTypes, truncateForLog(content.String(), 700))
	}
	items = ensureFiveSuggestions(query, items)
	if err := writeSuggestionsCache(query, items); err != nil {
		fmt.Printf("[Morph] suggestions cache write failed requestID=%s error=%v\n", requestID, err)
	}
	for _, item := range items {
		key := suggestionKey(item)
		if key == "" || emitted[key] {
			continue
		}
		emitted[key] = true
		copied := item
		a.emit("suggestions:item", suggestionEvent{
			RequestID: requestID,
			TabID:     tabID,
			Query:     query,
			Item:      &copied,
		})
	}
	fmt.Printf("[Morph] suggestions request completed requestID=%s items=%d eventTypes=%v\n", requestID, len(items), seenEventTypes)
	a.emit("suggestions:done", suggestionEvent{
		RequestID: requestID,
		TabID:     tabID,
		Query:     query,
		Items:     items,
	})
}

func (a *App) emitSuggestionError(requestID string, tabID int64, query string, err error) {
	logRequestProblem(requestID, query, err)
	a.emit("suggestions:error", suggestionEvent{
		RequestID: requestID,
		TabID:     tabID,
		Query:     query,
		Message:   err.Error(),
	})
}

func (a *App) emit(eventName string, data suggestionEvent) {
	if a.ctx == nil {
		return
	}
	runtime.EventsEmit(a.ctx, eventName, data)
}

func (a *App) emitPage(eventName string, data pageEvent) {
	if a.ctx == nil {
		return
	}
	runtime.EventsEmit(a.ctx, eventName, data)
}

func extractSuggestions(content string) []suggestion {
	arrayStart := strings.Index(content, "[")
	if arrayStart == -1 {
		return nil
	}

	var items []suggestion
	inString := false
	escaped := false
	depth := 0
	objectStart := -1

	for index := arrayStart; index < len(content); index++ {
		character := content[index]
		if escaped {
			escaped = false
			continue
		}
		if character == '\\' && inString {
			escaped = true
			continue
		}
		if character == '"' {
			inString = !inString
			continue
		}
		if inString {
			continue
		}

		switch character {
		case '{':
			if depth == 0 {
				objectStart = index
			}
			depth++
		case '}':
			if depth == 0 {
				continue
			}
			depth--
			if depth == 0 && objectStart >= 0 {
				var item suggestion
				if err := json.Unmarshal([]byte(content[objectStart:index+1]), &item); err == nil && item.Title != "" && item.Description != "" {
					items = append(items, normalizeSuggestion(item))
					if len(items) >= maxSuggestions {
						return items
					}
				}
				objectStart = -1
			}
		}
	}

	return items
}

func normalizeSuggestion(item suggestion) suggestion {
	item.ID = strings.TrimSpace(item.ID)
	item.Title = strings.TrimSpace(item.Title)
	item.Description = strings.TrimSpace(item.Description)
	item.Kind = strings.TrimSpace(item.Kind)
	item.Query = strings.TrimSpace(item.Query)

	if item.ID == "" {
		item.ID = strings.ToLower(strings.ReplaceAll(item.Title, " ", "-"))
	}
	if item.Kind == "" {
		item.Kind = "generated_app"
	}
	if item.Query == "" {
		item.Query = item.Title
	}

	return item
}

func textFromStreamEvent(event responseStreamEvent) string {
	if event.Type == "response.output_text.delta" && event.Delta != "" {
		return event.Delta
	}
	if event.Type == "response.output_text.done" && event.Text != "" {
		return event.Text
	}
	if event.Type == "response.output_item.done" && event.Item != nil {
		return textFromOutput(*event.Item)
	}
	if event.Type != "response.completed" || event.Response == nil {
		return ""
	}

	var builder strings.Builder
	for _, output := range event.Response.Output {
		builder.WriteString(textFromOutput(output))
	}
	return builder.String()
}

func textFromOutput(output responseOutput) string {
	var builder strings.Builder
	for _, content := range output.Content {
		if content.Text != "" {
			builder.WriteString(content.Text)
		}
	}
	return builder.String()
}

func incompleteReason(event responseStreamEvent) string {
	if event.IncompleteDetails != nil && event.IncompleteDetails.Reason != "" {
		return event.IncompleteDetails.Reason
	}
	if event.Response != nil {
		return "see response payload"
	}
	return "unknown"
}

func ensureFiveSuggestions(query string, items []suggestion) []suggestion {
	result := make([]suggestion, 0, maxSuggestions)
	seen := map[string]bool{}

	for _, item := range items {
		item = normalizeSuggestion(item)
		key := suggestionKey(item)
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		result = append(result, item)
		if len(result) == maxSuggestions {
			return result
		}
	}

	for _, item := range fallbackSuggestions(query) {
		key := suggestionKey(item)
		if key == "" || seen[key] {
			continue
		}
		seen[key] = true
		result = append(result, item)
		if len(result) == maxSuggestions {
			return result
		}
	}

	return result
}

func fallbackSuggestions(query string) []suggestion {
	cleanQuery := strings.TrimSpace(query)
	if cleanQuery == "" {
		cleanQuery = "new idea"
	}

	slug := strings.ToLower(strings.ReplaceAll(cleanQuery, " ", "-"))
	slug = strings.Trim(slug, "-")
	if slug == "" {
		slug = "idea"
	}

	return []suggestion{
		{
			ID:          slug + "-dreamlab",
			Title:       titleCase(cleanQuery) + " Dream Lab",
			Description: "A cinematic generated experience that remixes the idea into an interactive world.",
			Kind:        "generated_app",
			Query:       "Create a wildly imaginative dream lab for " + cleanQuery,
		},
		{
			ID:          slug + "-studio",
			Title:       titleCase(cleanQuery) + " Signal Studio",
			Description: "A polished creative control room with strange tools, motion, and generated panels.",
			Kind:        "tool",
			Query:       "Generate a creative signal studio for " + cleanQuery,
		},
		{
			ID:          slug + "-arcade",
			Title:       titleCase(cleanQuery) + " Arcade",
			Description: "A playful generated interface with game-like controls and visual surprises.",
			Kind:        "generated_app",
			Query:       "Build an arcade-style generated page for " + cleanQuery,
		},
		{
			ID:          slug + "-atlas",
			Title:       titleCase(cleanQuery) + " Myth Atlas",
			Description: "A beautiful explorer that maps the topic as places, artifacts, and living cards.",
			Kind:        "website",
			Query:       "Open a myth-atlas creative explorer for " + cleanQuery,
		},
		{
			ID:          slug + "-machine",
			Title:       titleCase(cleanQuery) + " Machine",
			Description: "A bold generated mini-site that turns the search into an experimental instrument.",
			Kind:        "generated_app",
			Query:       "Create an experimental machine interface for " + cleanQuery,
		},
	}
}

func practicalSuggestion(query string) suggestion {
	cleanQuery := strings.TrimSpace(query)
	if cleanQuery == "" {
		cleanQuery = "new app"
	}
	lower := strings.ToLower(cleanQuery)
	slug := strings.Trim(strings.ToLower(strings.ReplaceAll(cleanQuery, " ", "-")), "-")
	if slug == "" {
		slug = "app"
	}

	switch {
	case strings.Contains(lower, "racing") && strings.Contains(lower, "game"):
		return suggestion{
			ID:          slug + "-playable",
			Title:       "Racing Game",
			Description: "A playable browser racing game with steering, laps, timer, and track UI.",
			Kind:        "generated_app",
			Query:       "Create a playable racing game",
		}
	case strings.Contains(lower, "game"):
		return suggestion{
			ID:          slug + "-game",
			Title:       titleCase(cleanQuery),
			Description: "A playable game interface matching the search, with controls and score.",
			Kind:        "generated_app",
			Query:       "Create a playable " + cleanQuery,
		}
	case strings.Contains(lower, "calculator"):
		return suggestion{
			ID:          "calculator-app",
			Title:       "Calculator",
			Description: "A clean working calculator with keypad, display, and basic operations.",
			Kind:        "generated_app",
			Query:       "Create a working calculator app",
		}
	case strings.Contains(lower, "todo") || strings.Contains(lower, "to-do") || strings.Contains(lower, "task"):
		return suggestion{
			ID:          slug + "-todo",
			Title:       "Todo List",
			Description: "A simple task list with add, complete, filter, and delete controls.",
			Kind:        "generated_app",
			Query:       "Create a working todo list app",
		}
	case strings.Contains(lower, "portfolio") || strings.Contains(lower, "profile") || strings.Contains(lower, "resume") || strings.Contains(lower, "social links"):
		return suggestion{
			ID:          slug + "-profile",
			Title:       titleCase(cleanQuery),
			Description: "A grounded profile or portfolio page matching the search directly.",
			Kind:        "generated_app",
			Query:       "Create " + cleanQuery,
		}
	case len(strings.Fields(cleanQuery)) >= 2 && !strings.Contains(lower, " app") && !strings.Contains(lower, "tool"):
		return suggestion{
			ID:          slug + "-search-page",
			Title:       titleCase(cleanQuery),
			Description: "A direct, grounded page for this name or phrase with concise sections.",
			Kind:        "generated_app",
			Query:       "Create a direct page for " + cleanQuery,
		}
	default:
		return suggestion{
			ID:          slug + "-app",
			Title:       titleCase(cleanQuery),
			Description: "A practical app or page that matches the search as directly as possible.",
			Kind:        "generated_app",
			Query:       "Create " + cleanQuery,
		}
	}
}

func suggestionKey(item suggestion) string {
	if item.ID != "" {
		return strings.ToLower(item.ID)
	}
	return strings.ToLower(item.Title)
}

func titleCase(value string) string {
	words := strings.Fields(value)
	for index, word := range words {
		if len(word) == 0 {
			continue
		}
		words[index] = strings.ToUpper(word[:1]) + strings.ToLower(word[1:])
	}
	return strings.Join(words, " ")
}

func logRequestProblem(requestID string, query string, err error) {
	if err == nil {
		return
	}
	fmt.Printf("[Morph] suggestion request problem requestID=%s query=%q error=%v\n", requestID, query, err)
}

func truncateForLog(value string, limit int) string {
	value = strings.TrimSpace(value)
	if len(value) <= limit {
		return value
	}
	return value[:limit] + "..."
}

func readSuggestionsCache(query string) ([]suggestion, bool) {
	path := cachePath("suggestions", query)
	bytes, err := os.ReadFile(path)
	if err != nil {
		return nil, false
	}

	var items []suggestion
	if err := json.Unmarshal(bytes, &items); err != nil {
		fmt.Printf("[Morph] suggestions cache read failed path=%s error=%v\n", path, err)
		return nil, false
	}
	return ensureFiveSuggestions(query, items), true
}

func writeSuggestionsCache(query string, items []suggestion) error {
	items = ensureFiveSuggestions(query, items)
	bytes, err := json.MarshalIndent(items, "", "  ")
	if err != nil {
		return err
	}
	return writeCacheFile(cachePath("suggestions", query), bytes)
}

func readPageCache(title string, description string, query string) (string, bool) {
	path := cachePath("pages", pageCacheKey(title, description, query))
	bytes, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	spec := strings.TrimSpace(string(bytes))
	if spec == "" {
		return "", false
	}

	var decoded map[string]interface{}
	if err := json.Unmarshal([]byte(spec), &decoded); err != nil {
		fmt.Printf("[Morph] page cache read failed path=%s error=%v\n", path, err)
		return "", false
	}
	return spec, true
}

func writePageCache(title string, description string, query string, spec string) error {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return nil
	}
	var decoded map[string]interface{}
	if err := json.Unmarshal([]byte(spec), &decoded); err != nil {
		return err
	}
	return writeCacheFile(cachePath("pages", pageCacheKey(title, description, query)), []byte(spec))
}

func writeCacheFile(path string, bytes []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	return os.WriteFile(path, bytes, 0644)
}

func cachePath(kind string, key string) string {
	sum := sha256.Sum256([]byte(kind + "\n" + cacheVersion + "\n" + normalizeCacheKey(key)))
	filename := hex.EncodeToString(sum[:]) + ".json"
	return filepath.Join(cacheDirName, kind, filename)
}

func pageCacheKey(title string, description string, query string) string {
	return strings.Join([]string{title, description, query}, "\n---\n")
}

func normalizeCacheKey(key string) string {
	return strings.Join(strings.Fields(strings.ToLower(strings.TrimSpace(key))), " ")
}

func sanitizeJSON(input string) string {
	var result strings.Builder
	inString := false
	escaped := false
	for i := 0; i < len(input); i++ {
		char := input[i]
		if escaped {
			result.WriteByte(char)
			escaped = false
			continue
		}
		if char == '\\' && inString {
			result.WriteByte(char)
			escaped = true
			continue
		}
		if char == '"' {
			inString = !inString
			result.WriteByte(char)
			continue
		}
		if inString {
			if char == '\n' {
				result.WriteString("\\n")
			} else if char == '\r' {
				result.WriteString("\\r")
			} else if char == '\t' {
				result.WriteString("\\t")
			} else {
				result.WriteByte(char)
			}
		} else {
			result.WriteByte(char)
		}
	}
	return result.String()
}

func extractJSONObject(content string) string {
	content = sanitizeJSON(content)
	start := strings.Index(content, "{")
	if start == -1 {
		return ""
	}

	inString := false
	escaped := false
	depth := 0
	for index := start; index < len(content); index++ {
		character := content[index]
		if escaped {
			escaped = false
			continue
		}
		if character == '\\' && inString {
			escaped = true
			continue
		}
		if character == '"' {
			inString = !inString
			continue
		}
		if inString {
			continue
		}
		if character == '{' {
			depth++
		}
		if character == '}' {
			depth--
			if depth == 0 {
				candidate := content[start : index+1]
				var decoded map[string]interface{}
				if err := json.Unmarshal([]byte(candidate), &decoded); err != nil {
					return ""
				}
				return candidate
			}
		}
	}
	return ""
}

func fallbackPageJSON(title string, description string, query string) string {
	if description == "" {
		description = "A generated Morph workspace built from the selected result."
	}
	if query == "" {
		query = title
	}

	spec := map[string]interface{}{
		"title":      title,
		"subtitle":   description,
		"mode":       "mini_app",
		"customHtml": "<main class=\"fallback\"><section><p class=\"eyebrow\">Generated fallback</p><h1>" + title + "</h1><p>" + description + "</p><button id=\"pulse\">Pulse idea</button><div id=\"notes\"></div></section></main>",
		"customCss":  ".fallback{min-height:100%;display:grid;place-items:center;padding:40px;background:radial-gradient(circle at 20% 10%,#8ab4f844,transparent 28%),#101114;color:#f5f7fb;font-family:Inter,Segoe UI,sans-serif}.fallback section{max-width:760px;border:1px solid rgba(255,255,255,.14);border-radius:28px;padding:36px;background:rgba(255,255,255,.06)}.eyebrow{color:#8ab4f8;text-transform:uppercase;font-size:12px;font-weight:800}.fallback h1{font-size:48px;margin:8px 0}.fallback p{color:#bdc1c6;line-height:1.6}.fallback button{height:42px;border:0;border-radius:999px;background:#8ab4f8;color:#101114;font-weight:800;padding:0 18px}#notes{margin-top:18px;color:#71e39f}",
		"customJs":   "let n=0;document.getElementById('pulse')?.addEventListener('click',()=>{n++;document.getElementById('notes').textContent='Generated signal '+n+' for " + query + "';});",
		"theme": map[string]string{
			"accent": "#8ab4f8",
			"mood":   "clean",
		},
		"sections": []map[string]interface{}{
			{
				"type":        "hero",
				"title":       title,
				"description": description,
				"actions": []map[string]string{
					{"label": "Start", "action": "highlight", "target": "hero"},
					{"label": "Add idea", "action": "append", "target": "notes"},
				},
			},
			{
				"type":  "stats",
				"title": "Signal",
				"items": []map[string]string{
					{"label": "Ideas", "value": "12"},
					{"label": "Views", "value": "4"},
					{"label": "Momentum", "value": "High"},
				},
			},
			{
				"type":        "cards",
				"title":       "Generated modules",
				"description": "Useful surfaces Morph can open from this result.",
				"items": []map[string]string{
					{"title": titleCase(query) + " Dashboard", "description": "A live overview with metrics and next actions."},
					{"title": titleCase(query) + " Planner", "description": "A compact board for steps, notes, and decisions."},
					{"title": titleCase(query) + " Gallery", "description": "A visual collection of generated assets and links."},
				},
			},
			{
				"type":  "controls",
				"title": "Quick controls",
				"actions": []map[string]string{
					{"label": "Increment score", "action": "increment", "target": "score"},
					{"label": "Toggle focus", "action": "toggle", "target": "focus"},
					{"label": "Add note", "action": "append", "target": "notes"},
				},
			},
			{
				"type":  "list",
				"title": "Next steps",
				"items": []map[string]string{
					{"title": "Sketch the main screen", "description": "Define the first view a user should land on."},
					{"title": "Add useful data", "description": "Create fields, cards, and lists that make the page functional."},
					{"title": "Wire interactions", "description": "Connect controls to local state and visible feedback."},
				},
			},
		},
	}

	bytes, err := json.Marshal(spec)
	if err != nil {
		return `{"title":"Generated Page","subtitle":"Fallback page","sections":[]}`
	}
	return string(bytes)
}

func readEnvValue(key string) string {
	if value := strings.TrimSpace(os.Getenv(key)); value != "" {
		return value
	}

	envPath, err := filepath.Abs(".env")
	if err != nil {
		return ""
	}

	file, err := os.Open(envPath)
	if err != nil {
		return ""
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") || !strings.Contains(line, "=") {
			continue
		}

		parts := strings.SplitN(line, "=", 2)
		if strings.TrimSpace(parts[0]) != key {
			continue
		}

		return strings.Trim(strings.TrimSpace(parts[1]), `"'`)
	}

	return ""
}

func newRequestID() string {
	bytes := make([]byte, 8)
	if _, err := rand.Read(bytes); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(bytes)
}
