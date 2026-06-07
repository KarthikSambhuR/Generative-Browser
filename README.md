# Generative Browser

## Overview
Generative Browser is a desktop AI browser built with Wails, Go, React, and TypeScript. It lets users search for an idea, choose between sourced or creative generation, and open generated websites, tools, dashboards, articles, and mini apps inside a browser-like interface.

## Problem Statement
Traditional search gives users links, but many queries need a synthesized, interactive answer instead of a list of pages. Users also need a way to quickly turn ideas into usable web experiences without manually designing layouts, writing code, or stitching together source material.

## Solution
Generative Browser combines web search, source scraping, and LLM-powered page generation. In sourced mode, it gathers DuckDuckGo results and source metadata, then creates a cited generated page. In creative mode, it generates imaginative app or website suggestions and turns a selected result into a custom interactive page.

## Features
- Sourced search mode that builds generated pages from DuckDuckGo sources and scraped context.
- Creative search mode that generates app, website, tool, dashboard, and mini-app ideas.
- Multiple generation speed modes using OpenAI, Cerebras, and Google Gemma.
- Embedded generated pages with custom HTML, CSS, and JavaScript.
- Browser-style tab UI with history, loading states, streaming progress, and generated page previews.
- Direct URL handling for live web pages and iframe fallback for protected sites.

## Tech Stack
- Frontend: React, TypeScript, Vite, Lucide React
- Backend: Go, Wails
- Database: Local filesystem cache
- APIs: OpenAI Responses API, Cerebras Chat Completions API, Google Gemini/Gemma API, DuckDuckGo HTML search
- Hosting: Desktop app via Wails

## Codex / OpenAI Usage
Codex was used throughout the build to help with:
- Ideation for the browser-like generated search experience
- Architecture planning for search modes, generation modes, and provider routing
- Code generation for Wails event streaming, React UI states, and Go API integrations
- Debugging provider-specific request and response formats
- Testing with Go and frontend build commands
- Documentation generation for this README
- API integration work for OpenAI, Cerebras, Google Gemma, and sourced search behavior

## Demo
Add your demo or pitch video link here.

## Screenshots
Add screenshots of your project here.

## How to Run Locally

```bash
git clone https://github.com/KarthikSambhuR/Generative-Browser
cd Generative-Browser
cd frontend
npm install
cd ..
wails dev
```

For a frontend-only development server:

```bash
cd frontend
npm install
npm run dev
```

Create a `.env` file in the project root with the API keys you want to use:

```bash
OPENAI_API_KEY=your_openai_api_key_here
CEREBRAS_API_KEY=your_cerebras_api_key_here
GEMINI_API_KEY=your_gemini_api_key_here
```
