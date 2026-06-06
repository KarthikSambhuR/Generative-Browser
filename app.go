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
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/wailsapp/wails/v2/pkg/runtime"
)

const openAIResponsesURL = "https://api.openai.com/v1/responses"
const maxSuggestions = 5
const cacheDirName = "cache"
const cacheVersion = "v3-ranked-creativity"

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
	Model           string          `json:"model"`
	Instructions    string          `json:"instructions"`
	Input           string          `json:"input"`
	MaxOutputTokens int             `json:"max_output_tokens"`
	Stream          bool            `json:"stream"`
	Reasoning       reasoningConfig `json:"reasoning"`
	Text            textConfig      `json:"text"`
}

type reasoningConfig struct {
	Effort string `json:"effort"`
}

type textConfig struct {
	Verbosity string `json:"verbosity"`
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
func (a *App) SearchSuggestions(query string, tabID int64, requestID string) (string, error) {
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

	apiKey := readEnvValue("OPENAI_API_KEY")
	if apiKey == "" {
		err := errors.New("OPENAI_API_KEY is missing; add it to .env or your shell environment")
		logRequestProblem(requestID, query, err)
		return "", err
	}

	go a.streamSuggestions(requestID, tabID, query, apiKey)

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
func (a *App) GeneratePageStream(title string, description string, query string, tabID int64, requestID string) (string, error) {
	title = strings.TrimSpace(title)
	description = strings.TrimSpace(description)
	query = strings.TrimSpace(query)
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

	if cached, ok := readPageCache(title, description, query); ok {
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

	apiKey := readEnvValue("OPENAI_API_KEY")
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

	go a.streamPage(requestID, tabID, title, description, query, apiKey)
	return requestID, nil
}

func (a *App) streamPage(requestID string, tabID int64, title string, description string, query string, apiKey string) {
	fmt.Printf("[Morph] page stream started requestID=%s tabID=%d title=%q query=%q provider=openai model=gpt-5-nano\n", requestID, tabID, title, query)
	a.emitPage("page:started", pageEvent{RequestID: requestID, TabID: tabID, Title: title})

	body := responsesRequest{
		Model:           "gpt-5-nano",
		Instructions:    "You are Morph's extreme creative page generator. Return only JSON. No markdown. Create a wildly custom, polished mini web app for the clicked result. Every page must have a unique visual layout and custom interface. Use modern design standards: clear hierarchy, responsive layout, accessible contrast, polished spacing, stable controls, and professional interaction states. If using fonts, load Google Fonts with @import in customCss. If icons are useful, use lucide-style inline SVG icons or simple line icons, not emoji. Include customHtml, customCss, and customJs. The JS must be self-contained browser JavaScript only; no imports, no network, no localStorage, no parent/window.top access. It may use DOM event listeners, timers, canvas, buttons, forms, generated data, animations, and local variables. Schema: {\"title\":\"string\",\"subtitle\":\"string\",\"mode\":\"mini_app|article|dashboard|tool|game|gallery\",\"customHtml\":\"string\",\"customCss\":\"string\",\"customJs\":\"string\"}. Make the HTML body fragment complete enough to be useful. If the search is simple, make a playful focused single-purpose interface instead of a generic page.",
		Input:           fmt.Sprintf("Clicked result title: %s\nDescription: %s\nOriginal query: %s\nGenerate a unique custom HTML/CSS/JS page JSON now.", title, description, query),
		MaxOutputTokens: 9000,
		Stream:          true,
		Reasoning: reasoningConfig{
			Effort: "minimal",
		},
		Text: textConfig{
			Verbosity: "low",
		},
	}

	payload, err := json.Marshal(body)
	if err != nil {
		a.emitPageError(requestID, tabID, title, query, err)
		return
	}

	req, err := http.NewRequestWithContext(a.ctx, http.MethodPost, openAIResponsesURL, bytes.NewReader(payload))
	if err != nil {
		a.emitPageError(requestID, tabID, title, query, err)
		return
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")

	client := &http.Client{Timeout: 120 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		a.emitPageError(requestID, tabID, title, query, err)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		responseBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		a.emitPageError(requestID, tabID, title, query, fmt.Errorf("OpenAI page stream returned %s: %s", resp.Status, strings.TrimSpace(string(responseBody))))
		return
	}

	content := strings.Builder{}
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
			fmt.Printf("[Morph] ignored malformed page stream event requestID=%s error=%v body=%s\n", requestID, err, truncateForLog(data, 500))
			continue
		}
		if event.Type != "" {
			seenEventTypes[event.Type]++
		}
		if event.Error != nil {
			a.emitPageError(requestID, tabID, title, query, fmt.Errorf("OpenAI page stream error type=%s code=%s message=%s", event.Error.Type, event.Error.Code, event.Error.Message))
			return
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
		a.emitPageError(requestID, tabID, title, query, err)
		return
	}

	spec := extractJSONObject(content.String())
	if spec == "" {
		fmt.Printf("[Morph] page stream returned no JSON; using fallback requestID=%s eventTypes=%v raw=%s\n", requestID, seenEventTypes, truncateForLog(content.String(), 900))
		spec = fallbackPageJSON(title, description, query)
	}
	fmt.Printf("[Morph] page stream completed requestID=%s bytes=%d eventTypes=%v\n", requestID, len(spec), seenEventTypes)
	if err := writePageCache(title, description, query, spec); err != nil {
		fmt.Printf("[Morph] page cache write failed requestID=%s error=%v\n", requestID, err)
	}
	a.emitPage("page:done", pageEvent{
		RequestID: requestID,
		TabID:     tabID,
		Title:     title,
		Spec:      spec,
	})
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
func (a *App) GeneratePage(title string, description string, query string) (string, error) {
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

	fmt.Printf("[Morph] page generation started title=%q query=%q provider=openai model=gpt-5-nano\n", title, query)

	body := responsesRequest{
		Model:           "gpt-5-nano",
		Instructions:    "You create compact JSON UI specs for Morph. Return only JSON. No markdown. The JSON must be safe and renderable with this schema: {\"title\":\"string\",\"subtitle\":\"string\",\"theme\":{\"accent\":\"#hex\",\"mood\":\"dark|clean|neon|calm\"},\"sections\":[{\"type\":\"hero|stats|cards|list|form|table|controls\",\"title\":\"string\",\"description\":\"string\",\"items\":[{\"title\":\"string\",\"description\":\"string\",\"value\":\"string\",\"label\":\"string\"}],\"fields\":[{\"label\":\"string\",\"placeholder\":\"string\"}],\"actions\":[{\"label\":\"string\",\"action\":\"increment|toggle|append|highlight\",\"target\":\"string\"}]}]}. Make the UI feel cool, functional, and specific to the title. Include 5 to 7 sections. Include at least one controls or form section and one stats/cards/list section.",
		Input:           fmt.Sprintf("Title: %s\nDescription: %s\nOriginal query: %s\nCreate the JSON UI page spec.", title, description, query),
		MaxOutputTokens: 5000,
		Stream:          false,
		Reasoning: reasoningConfig{
			Effort: "minimal",
		},
		Text: textConfig{
			Verbosity: "low",
		},
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

func (a *App) streamSuggestions(requestID string, tabID int64, query string, apiKey string) {
	fmt.Printf("[Morph] suggestions request started requestID=%s tabID=%d query=%q provider=openai model=gpt-5-nano\n", requestID, tabID, query)

	a.emit("suggestions:started", suggestionEvent{
		RequestID: requestID,
		TabID:     tabID,
		Query:     query,
	})

	body := responsesRequest{
		Model:           "gpt-5-nano",
		Instructions:    "You are Morph's ranked suggestion engine. Return only JSON. No markdown. No prose. Shape: {\"suggestions\":[{\"id\":\"short-kebab-id\",\"title\":\"App or website title\",\"description\":\"One short sentence about what it opens or creates.\",\"kind\":\"website|generated_app|tool\",\"query\":\"actionable query to run\"}]}. Return exactly 5 suggestions in one JSON object. Ranking rule: suggestion 1 must be the most literal, practical, and least creative interpretation of the user's search. For simple searches, names, people, portfolios, resumes, brands, local projects, or direct app requests, suggestion 1 should be something Morph can actually implement directly, like a profile page, portfolio, simple app, dashboard, search page, or practical website. Suggestions 2-4 should be more creative than suggestion 1 but still plausible and useful on average. Suggestion 5 can be very creative, speculative, or playful. For topics that do not exist yet or are future/fictional, like unreleased games, keep suggestion 1 as a practical tracker/info hub and put bigger creative ideas later.",
		Input:           fmt.Sprintf("User searched: %q. Suggest available apps, generated app ideas, or websites that match.", query),
		MaxOutputTokens: 3500,
		Stream:          true,
		Reasoning: reasoningConfig{
			Effort: "minimal",
		},
		Text: textConfig{
			Verbosity: "low",
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
			ID:          slug + "-studio",
			Title:       titleCase(cleanQuery) + " Studio",
			Description: "A focused workspace that turns the idea into a clean, usable mini app.",
			Kind:        "generated_app",
			Query:       "Create a polished " + cleanQuery + " studio app",
		},
		{
			ID:          slug + "-dashboard",
			Title:       titleCase(cleanQuery) + " Dashboard",
			Description: "A compact command center with cards, lists, progress, and quick actions.",
			Kind:        "generated_app",
			Query:       "Build a dashboard for " + cleanQuery,
		},
		{
			ID:          slug + "-atlas",
			Title:       titleCase(cleanQuery) + " Atlas",
			Description: "A visual explorer that organizes related tools, notes, and inspirations.",
			Kind:        "tool",
			Query:       "Open an atlas-style explorer for " + cleanQuery,
		},
		{
			ID:          slug + "-brief",
			Title:       titleCase(cleanQuery) + " Brief",
			Description: "A fast briefing page that converts the topic into next steps and options.",
			Kind:        "generated_app",
			Query:       "Generate a smart brief for " + cleanQuery,
		},
		{
			ID:          slug + "-portal",
			Title:       titleCase(cleanQuery) + " Portal",
			Description: "A browser-like launch page for the apps and sites Morph could open next.",
			Kind:        "website",
			Query:       "Open a creative portal for " + cleanQuery,
		},
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

func extractJSONObject(content string) string {
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
