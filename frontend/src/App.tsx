import { CSSProperties, FormEvent, KeyboardEvent, MouseEvent, UIEvent, useEffect, useMemo, useRef, useState } from 'react';
import {
    Search,
    Plus,
    X,
    Minus,
    Square,
    ArrowRight,
    ArrowLeft,
    Copy,
    ExternalLink,
    Loader2
} from 'lucide-react';
import './App.css';
import {GeneratePageStream, SearchSuggestions} from '../wailsjs/go/main/App';
import {ClipboardSetText, EventsOn, Quit, WindowMinimise, WindowToggleMaximise} from '../wailsjs/runtime';

type TabKind = 'search' | 'generated';
type SearchMode = 'sourced' | 'creative';
type GenerationMode = 'standard' | 'superfast';
type SuggestionStatus = 'idle' | 'loading' | 'done' | 'error';
type PageStatus = 'idle' | 'loading' | 'ready' | 'error';

type AppSuggestion = {
    id: string;
    title: string;
    description: string;
    kind: string;
    query: string;
};

type SuggestionEvent = {
    requestId: string;
    tabId: number;
    query: string;
    item?: AppSuggestion;
    items?: AppSuggestion[];
    message?: string;
};

type PageEvent = {
    requestId: string;
    tabId: number;
    title: string;
    chunk?: string;
    spec?: string;
    message?: string;
};

type Tab = {
    id: number;
    title: string;
    query: string;
    kind: TabKind;
    searchMode: SearchMode;
    generationMode: GenerationMode;
    createdAt: string;
    suggestions: AppSuggestion[];
    suggestionStatus: SuggestionStatus;
    suggestionError?: string;
    requestId?: string;
    visibleResults: number;
    pageStatus: PageStatus;
    pageSpec?: PageSpec;
    pageError?: string;
    pageStream?: string;
    history: TabHistoryEntry[];
};

const maxSuggestions = 5;
const initialVisibleResults = 12;
const resultBatchSize = 8;

type SearchResult = AppSuggestion & {
    url: string;
};

type PageItem = {
    title?: string;
    description?: string;
    value?: string;
    label?: string;
};

type PageField = {
    label: string;
    placeholder?: string;
};

type PageAction = {
    label: string;
    action: 'increment' | 'toggle' | 'append' | 'highlight';
    target: string;
};

type PageSection = {
    type: 'hero' | 'stats' | 'cards' | 'list' | 'form' | 'table' | 'controls';
    title?: string;
    description?: string;
    items?: PageItem[];
    fields?: PageField[];
    actions?: PageAction[];
};

type PageSpec = {
    title: string;
    subtitle?: string;
    mode?: string;
    sourceUrl?: string;
    customHtml?: string;
    customCss?: string;
    customJs?: string;
    theme?: {
        accent?: string;
        mood?: string;
    };
    sections: PageSection[];
};

type TabHistoryEntry = {
    title: string;
    query: string;
    kind: TabKind;
    searchMode: SearchMode;
    generationMode: GenerationMode;
    suggestions: AppSuggestion[];
    suggestionStatus: SuggestionStatus;
    suggestionError?: string;
    visibleResults: number;
    pageStatus: PageStatus;
    pageSpec?: PageSpec;
    pageError?: string;
    pageStream?: string;
};

type ContextMenuState = {
    x: number;
    y: number;
    result: SearchResult;
} | null;

const starterTabs: Tab[] = [
    {
        id: 1,
        title: 'New search',
        query: '',
        kind: 'search',
        searchMode: 'sourced',
        generationMode: 'standard',
        createdAt: 'Just now',
        suggestions: [],
        suggestionStatus: 'idle',
        visibleResults: initialVisibleResults,
        pageStatus: 'idle',
        history: [],
    },
];

function classifyQuery(query: string): TabKind {
    const normalized = query.toLowerCase();
    const generateWords = ['create', 'generate', 'build', 'make', 'design'];
    return generateWords.some((word) => normalized.includes(word)) ? 'generated' : 'search';
}

function pageModeQuery(mode: SearchMode, query: string) {
    return `__generative_browser_mode:${mode}__\n${query}`;
}

function titleFromQuery(query: string) {
    if (!query.trim()) {
        return 'New search';
    }

    return query.trim().length > 22 ? `${query.trim().slice(0, 22)}...` : query.trim();
}

function newRequestID() {
    if (crypto.randomUUID) {
        return crypto.randomUUID();
    }
    return `${Date.now()}-${Math.random().toString(16).slice(2)}`;
}

function slugify(value: string) {
    return value
        .toLowerCase()
        .replace(/[^a-z0-9]+/g, '-')
        .replace(/^-|-$/g, '') || 'generative-browser-result';
}

function sourceFromKind(kind: string) {
    if (kind === 'website') {
        return 'www.generative-browser-web.local';
    }
    if (kind === 'tool') {
        return 'tools.generative-browser.local';
    }
    return 'apps.generative-browser.local';
}

function resultURL(result: AppSuggestion, index: number) {
    return `https://${sourceFromKind(result.kind)}/${slugify(result.title)}-${index + 1}`;
}

function expandResults(suggestions: AppSuggestion[], query: string): SearchResult[] {
    const suffixes = [
        'Overview',
        'Workspace',
        'Templates',
        'Live Preview',
        'Creator Mode',
        'Quick Start',
        'Examples',
        'Ideas',
        'Dashboard',
        'Playground',
    ];

    const base = suggestions.length > 0 ? suggestions : [];
    const results: SearchResult[] = [];

    base.forEach((suggestion, index) => {
        results.push({
            ...suggestion,
            url: resultURL(suggestion, index),
        });
    });

    for (let index = 0; index < 35 && base.length > 0; index++) {
        const seed = base[index % base.length];
        const suffix = suffixes[index % suffixes.length];
        const title = `${seed.title} ${suffix}`;
        results.push({
            id: `${seed.id}-${slugify(suffix)}-${index}`,
            title,
            description: `${seed.description} Includes a ${suffix.toLowerCase()} view for "${query}" with generated content and quick actions.`,
            kind: seed.kind,
            query: `${seed.query} ${suffix}`,
            url: resultURL({...seed, title}, index + base.length),
        });
    }

    return results;
}

function parsePartialJSONSpec(stream: string): PageSpec {
    const spec: PageSpec = {
        title: 'Generating...',
        sections: []
    };
    
    const extractStringKey = (key: string): string | undefined => {
        const keyPattern = new RegExp(`"${key}"\\s*:\\s*"`);
        const match = stream.match(keyPattern);
        if (!match || match.index === undefined) {
            return undefined;
        }
        
        const startIndex = match.index + match[0].length;
        let result = '';
        let escaped = false;
        
        for (let i = startIndex; i < stream.length; i++) {
            const char = stream[i];
            if (escaped) {
                if (char === 'n') result += '\n';
                else if (char === 't') result += '\t';
                else if (char === 'r') result += '\r';
                else if (char === 'b') result += '\b';
                else if (char === 'f') result += '\f';
                else result += char;
                escaped = false;
            } else if (char === '\\') {
                escaped = true;
            } else if (char === '"') {
                return result;
            } else {
                result += char;
            }
        }
        return result;
    };

    const customHtml = extractStringKey('customHtml');
    if (customHtml !== undefined) spec.customHtml = customHtml;
    
    const customCss = extractStringKey('customCss');
    if (customCss !== undefined) spec.customCss = customCss;
    
    const customJs = extractStringKey('customJs');
    if (customJs !== undefined) spec.customJs = customJs;
    
    const title = extractStringKey('title');
    if (title !== undefined) spec.title = title;
    
    const subtitle = extractStringKey('subtitle');
    if (subtitle !== undefined) spec.subtitle = subtitle;
    
    const mode = extractStringKey('mode');
    if (mode !== undefined) spec.mode = mode;
    
    return spec;
}

function normalizePageSpec(raw: unknown, result: SearchResult): PageSpec {
    const value = raw && typeof raw === 'object' ? raw as Partial<PageSpec> : {};
    const sections = Array.isArray(value.sections) ? value.sections : fallbackPageSpec(result).sections;
    return {
        title: typeof value.title === 'string' && value.title.trim() ? value.title : result.title,
        subtitle: typeof value.subtitle === 'string' ? value.subtitle : result.description,
        mode: typeof value.mode === 'string' ? value.mode : 'mini_app',
        sourceUrl: typeof value.sourceUrl === 'string' ? value.sourceUrl : generatedPageUrl(result),
        customHtml: typeof value.customHtml === 'string' ? value.customHtml : undefined,
        customCss: typeof value.customCss === 'string' ? value.customCss : undefined,
        customJs: typeof value.customJs === 'string' ? value.customJs : undefined,
        theme: {
            accent: value.theme?.accent || '#8ab4f8',
            mood: value.theme?.mood || 'clean',
        },
        sections: sections
            .filter((section) => section && typeof section === 'object')
            .slice(0, 8)
            .map((section) => ({
                type: section.type || 'cards',
                title: section.title,
                description: section.description,
                items: Array.isArray(section.items) ? section.items.slice(0, 8) : [],
                fields: Array.isArray(section.fields) ? section.fields.slice(0, 5) : [],
                actions: Array.isArray(section.actions) ? section.actions.slice(0, 5) : [],
            })),
    };
}

function tabToResult(tab: Tab): SearchResult {
    const title = tab.title || tab.query || 'Generated page';
        return {
            id: `${tab.id}`,
            title,
            description: tab.pageSpec?.subtitle || 'A generated Generative Browser page.',
            kind: tab.kind,
        query: tab.query || tab.title,
        url: generatedPageUrl({title, query: tab.query || title, kind: tab.kind}),
    };
}

function generatedPageUrl(result: SearchResult | {title: string; query: string; kind?: string}) {
    const host = result.kind === 'website' ? 'www.generative-browser.local' : 'apps.generative-browser.local';
    return `https://${host}/${slugify(result.title || result.query)}`;
}

function fallbackPageSpec(result: SearchResult): PageSpec {
    return {
        title: result.title,
        subtitle: result.description,
        mode: 'mini_app',
        sourceUrl: generatedPageUrl(result),
        customHtml: `<main class="fallback"><section><p class="eyebrow">Generated fallback</p><h1>${result.title}</h1><p>${result.description}</p><button id="pulse">Pulse idea</button><div id="notes"></div></section></main>`,
        customCss: `.fallback{min-height:100%;display:grid;place-items:center;padding:40px;background:radial-gradient(circle at 20% 10%,#8ab4f844,transparent 28%),#101114;color:#f5f7fb;font-family:Inter,Segoe UI,sans-serif}.fallback section{max-width:760px;border:1px solid rgba(255,255,255,.14);border-radius:28px;padding:36px;background:rgba(255,255,255,.06)}.eyebrow{color:#8ab4f8;text-transform:uppercase;font-size:12px;font-weight:800}.fallback h1{font-size:48px;margin:8px 0}.fallback p{color:#bdc1c6;line-height:1.6}.fallback button{height:42px;border:0;border-radius:999px;background:#8ab4f8;color:#101114;font-weight:800;padding:0 18px}#notes{margin-top:18px;color:#71e39f}`,
        customJs: `let n=0;document.getElementById('pulse')?.addEventListener('click',()=>{n++;document.getElementById('notes').textContent='Generated signal '+n;});`,
        theme: {accent: '#8ab4f8', mood: 'clean'},
        sections: [
            {
                type: 'hero',
                title: result.title,
                description: result.description,
                actions: [
                    {label: 'Highlight', action: 'highlight', target: 'hero'},
                    {label: 'Add note', action: 'append', target: 'notes'},
                ],
            },
            {
                type: 'stats',
                title: 'Live pulse',
                items: [
                    {label: 'Score', value: '12'},
                    {label: 'Ideas', value: '5'},
                    {label: 'Mode', value: 'Creative'},
                ],
            },
            {
                type: 'cards',
                title: 'Modules',
                items: [
                    {title: 'Dashboard', description: 'A quick overview generated for this result.'},
                    {title: 'Planner', description: 'A working space for actions and next steps.'},
                    {title: 'Gallery', description: 'A visual lane for assets and inspiration.'},
                ],
            },
            {
                type: 'controls',
                title: 'Controls',
                actions: [
                    {label: 'Increment score', action: 'increment', target: 'score'},
                    {label: 'Toggle focus', action: 'toggle', target: 'focus'},
                    {label: 'Append note', action: 'append', target: 'notes'},
                ],
            },
        ],
    };
}

function tabToHistoryEntry(tab: Tab): TabHistoryEntry {
    return {
        title: tab.title,
        query: tab.query,
        kind: tab.kind,
        searchMode: tab.searchMode,
        generationMode: tab.generationMode,
        suggestions: tab.suggestions,
        suggestionStatus: tab.suggestionStatus,
        suggestionError: tab.suggestionError,
        visibleResults: tab.visibleResults,
        pageStatus: tab.pageStatus,
        pageSpec: tab.pageSpec,
        pageError: tab.pageError,
        pageStream: tab.pageStream,
    };
}

function App() {
    const [tabs, setTabs] = useState<Tab[]>(starterTabs);
    const [activeTabId, setActiveTabId] = useState(1);
    const [command, setCommand] = useState('');
    const [contextMenu, setContextMenu] = useState<ContextMenuState>(null);

    const activeTab = useMemo(
        () => tabs.find((tab) => tab.id === activeTabId) ?? tabs[0],
        [activeTabId, tabs],
    );
    const isActiveTabLoading = activeTab.suggestionStatus === 'loading' || activeTab.pageStatus === 'loading';
    const canGoBack = activeTab.history.length > 0;

    const hasIframe = useMemo(() => {
        if (activeTab.kind !== 'generated') return false;
        if (activeTab.pageSpec?.customHtml) return true;
        return false;
    }, [activeTab.kind, activeTab.pageSpec]);

    const searchResults = useMemo(
        () => expandResults(activeTab.suggestions, activeTab.query),
        [activeTab.query, activeTab.suggestions],
    );

    const visibleResults = searchResults.slice(0, activeTab.visibleResults);

    function addTab() {
        const nextId = Date.now();
        const newTab: Tab = {
            id: nextId,
            title: 'New search',
            query: '',
            kind: 'search',
            searchMode: activeTab?.searchMode || 'sourced',
            generationMode: activeTab?.generationMode || 'standard',
            createdAt: 'Just now',
            suggestions: [],
            suggestionStatus: 'idle',
            visibleResults: initialVisibleResults,
            pageStatus: 'idle',
            history: [],
        };

        setTabs((currentTabs) => [...currentTabs, newTab]);
        setActiveTabId(nextId);
        setCommand('');
    }

    function closeTab(tabId: number) {
        if (tabs.length === 1) {
            Quit();
            return;
        }

        setTabs((currentTabs) => {
            const nextTabs = currentTabs.filter((tab) => tab.id !== tabId);
            if (activeTabId === tabId) {
                setActiveTabId(nextTabs[nextTabs.length - 1].id);
                setCommand(nextTabs[nextTabs.length - 1].query);
            }
            return nextTabs;
        });
    }

    function selectTab(tab: Tab) {
        setActiveTabId(tab.id);
        setCommand(tab.query);
        setContextMenu(null);
    }

    function handleTabKeyDown(event: KeyboardEvent<HTMLDivElement>, tab: Tab) {
        if (event.key === 'Enter' || event.key === ' ') {
            event.preventDefault();
            selectTab(tab);
        }
    }

    function startSearch(tabId: number, query: string) {
        const trimmedQuery = query.trim();
        if (!trimmedQuery) {
            return;
        }

        const requestId = newRequestID();
        const tab = tabs.find((t) => t.id === tabId);
        const selectedMode = tab?.searchMode || 'sourced';
        const generationMode = tab?.generationMode || 'standard';

        if (selectedMode === 'sourced') {
            const result: SearchResult = {
                id: `sourced-${slugify(trimmedQuery)}`,
                title: trimmedQuery,
                description: 'A sourced page built from DuckDuckGo results and scraped source metadata.',
                kind: 'website',
                query: trimmedQuery,
                url: `https://sources.generative-browser.local/${slugify(trimmedQuery)}`,
            };
            loadPageInTab(tabId, result, true, 'sourced');
            return;
        }

        setTabs((currentTabs) =>
            currentTabs.map((t) =>
                t.id === tabId
                    ? {
                        ...t,
                        history: [...t.history, tabToHistoryEntry(t)],
                        title: titleFromQuery(trimmedQuery),
                        query: trimmedQuery,
                        kind: 'search',
                        searchMode: 'creative',
                        suggestions: [],
                        suggestionStatus: 'loading',
                        suggestionError: undefined,
                        requestId,
                        visibleResults: initialVisibleResults,
                        pageStatus: 'idle',
                        pageSpec: undefined,
                        pageError: undefined,
                    }
                    : t,
            ),
        );

        SearchSuggestions(trimmedQuery, tabId, requestId, generationMode).catch((error) => {
            setTabs((currentTabs) =>
                currentTabs.map((t) =>
                    t.id === tabId && t.requestId === requestId
                        ? {
                            ...t,
                            suggestionStatus: 'error',
                            suggestionError: error instanceof Error ? error.message : String(error),
                        }
                        : t,
                ),
            );
        });
    }

    function loadPageInTab(tabId: number, result: SearchResult, pushHistory = true, mode: SearchMode = 'creative') {
        const requestId = newRequestID();
        const tab = tabs.find((t) => t.id === tabId);
        const generationMode = tab?.generationMode || 'standard';

        setTabs((currentTabs) =>
            currentTabs.map((t) =>
                t.id === tabId
                    ? {
                        ...t,
                        history: pushHistory ? [...t.history, tabToHistoryEntry(t)] : t.history,
                        title: titleFromQuery(result.title),
                        query: result.query,
                        kind: 'generated',
                        searchMode: mode,
                        requestId,
                        pageStatus: 'loading',
                        pageSpec: undefined,
                        pageError: undefined,
                        pageStream: '',
                    }
                    : t,
            ),
        );

        GeneratePageStream(result.title, result.description, pageModeQuery(mode, result.query), tabId, requestId, generationMode)
            .catch((error) => {
                setTabs((currentTabs) =>
                    currentTabs.map((t) =>
                        t.id === tabId && t.requestId === requestId
                            ? {
                                ...t,
                                pageStatus: 'error',
                                pageError: error instanceof Error ? error.message : String(error),
                                pageSpec: fallbackPageSpec(result),
                            }
                            : t,
                    ),
                );
            });
    }

    function openInSameTab(result: SearchResult) {
        setContextMenu(null);
        setCommand(result.query);
        loadPageInTab(activeTabId, result, true, activeTab.searchMode === 'sourced' ? 'sourced' : 'creative');
    }

    function openInNewTab(result: SearchResult) {
        setContextMenu(null);
        const nextId = Date.now();
        const requestId = newRequestID();
        const newTab: Tab = {
            id: nextId,
            title: titleFromQuery(result.query),
            query: result.query,
            kind: 'generated',
            searchMode: activeTab.searchMode === 'sourced' ? 'sourced' : 'creative',
            generationMode: activeTab.generationMode || 'standard',
            createdAt: 'Just now',
            suggestions: [],
            suggestionStatus: 'idle',
            suggestionError: undefined,
            requestId,
            visibleResults: initialVisibleResults,
            pageStatus: 'loading',
            history: [],
        };

        setTabs((currentTabs) => [...currentTabs, newTab]);
        setActiveTabId(nextId);
        setCommand(result.query);
        loadPageInTab(nextId, result, false, newTab.searchMode);
    }

    function setSearchMode(mode: SearchMode) {
        setTabs((currentTabs) =>
            currentTabs.map((tab) =>
                tab.id === activeTabId
                    ? {
                        ...tab,
                        searchMode: mode,
                        suggestionStatus: tab.query ? tab.suggestionStatus : 'idle',
                    }
                    : tab,
            ),
        );
    }

    function setGenerationMode(mode: GenerationMode) {
        setTabs((currentTabs) =>
            currentTabs.map((tab) =>
                tab.id === activeTabId
                    ? {
                        ...tab,
                        generationMode: mode,
                    }
                    : tab,
            ),
        );
    }

    function goBack() {
        const currentTab = tabs.find((tab) => tab.id === activeTabId);
        if (!currentTab || currentTab.history.length === 0) {
            return;
        }

        const previous = currentTab.history[currentTab.history.length - 1];
        setTabs((currentTabs) =>
            currentTabs.map((tab) =>
                tab.id === activeTabId
                    ? {
                        ...tab,
                        ...previous,
                        requestId: undefined,
                        history: tab.history.slice(0, -1),
                    }
                    : tab,
            ),
        );
        setCommand(previous.query);
        setContextMenu(null);
    }

    function handleResultMouseDown(event: MouseEvent<HTMLElement>, result: SearchResult) {
        if (event.button === 1) {
            event.preventDefault();
            openInNewTab(result);
        }
    }

    function handleResultContextMenu(event: MouseEvent<HTMLElement>, result: SearchResult) {
        event.preventDefault();
        setContextMenu({x: event.clientX, y: event.clientY, result});
    }

    function handleContentScroll(event: UIEvent<HTMLElement>) {
        const target = event.currentTarget;
        const distanceFromBottom = target.scrollHeight - target.scrollTop - target.clientHeight;
        if (distanceFromBottom > 220 || visibleResults.length >= searchResults.length) {
            return;
        }

        setTabs((currentTabs) =>
            currentTabs.map((tab) =>
                tab.id === activeTabId
                    ? {...tab, visibleResults: Math.min(tab.visibleResults + resultBatchSize, searchResults.length)}
                    : tab,
            ),
        );
    }

    function copyResult(result: SearchResult) {
        ClipboardSetText(`${result.title}\n${result.url}\n${result.description}`);
        setContextMenu(null);
    }

    useEffect(() => {
        const offStarted = EventsOn('suggestions:started', (event: SuggestionEvent) => {
            setTabs((currentTabs) =>
                currentTabs.map((tab) =>
                    tab.id === event.tabId && tab.requestId === event.requestId
                        ? {...tab, suggestionStatus: 'loading', suggestionError: undefined, suggestions: []}
                        : tab,
                ),
            );
        });

        const offItem = EventsOn('suggestions:item', (event: SuggestionEvent) => {
            if (!event.item) {
                return;
            }
            const incomingItem = event.item;

            setTabs((currentTabs) =>
                currentTabs.map((tab) => {
                    if (tab.id !== event.tabId || tab.requestId !== event.requestId) {
                        return tab;
                    }

                    const exists = tab.suggestions.some((item) => item.id === incomingItem.id);
                    return {
                        ...tab,
                        suggestionStatus: 'loading',
                        suggestions: exists ? tab.suggestions : [...tab.suggestions, incomingItem].slice(0, maxSuggestions),
                    };
                }),
            );
        });

        const offError = EventsOn('suggestions:error', (event: SuggestionEvent) => {
            setTabs((currentTabs) =>
                currentTabs.map((tab) =>
                    tab.id === event.tabId && tab.requestId === event.requestId
                        ? {
                            ...tab,
                            suggestionStatus: 'error',
                            suggestionError: event.message ?? 'Suggestion request failed.',
                        }
                        : tab,
                ),
            );
        });

        const offDone = EventsOn('suggestions:done', (event: SuggestionEvent) => {
            setTabs((currentTabs) =>
                currentTabs.map((tab) => {
                    if (tab.id !== event.tabId || tab.requestId !== event.requestId) {
                        return tab;
                    }

                    const merged = [...tab.suggestions];
                    for (const item of event.items ?? []) {
                        if (!merged.some((existing) => existing.id === item.id)) {
                            merged.push(item);
                        }
                    }

                    return {...tab, suggestions: merged.slice(0, maxSuggestions), suggestionStatus: 'done'};
                }),
            );
        });

        const offPageStarted = EventsOn('page:started', (event: PageEvent) => {
            setTabs((currentTabs) =>
                currentTabs.map((tab) =>
                    tab.id === event.tabId && tab.requestId === event.requestId
                        ? {
                            ...tab,
                            pageStream: '',
                        }
                        : tab,
                ),
            );
        });

        const offPageChunk = EventsOn('page:chunk', (event: PageEvent) => {
            setTabs((currentTabs) =>
                currentTabs.map((tab) =>
                    tab.id === event.tabId && tab.requestId === event.requestId
                        ? {
                            ...tab,
                            pageStatus: 'loading',
                            pageStream: `${tab.pageStream || ''}${event.chunk || ''}`,
                        }
                        : tab,
                ),
            );
        });

        const offPageDone = EventsOn('page:done', (event: PageEvent) => {
            setTabs((currentTabs) =>
                currentTabs.map((tab) => {
                    if (tab.id !== event.tabId || tab.requestId !== event.requestId) {
                        return tab;
                    }

                    try {
                        const fallbackResult = tabToResult(tab);
                        const parsed = normalizePageSpec(JSON.parse(event.spec || '{}'), fallbackResult);
                        return {...tab, pageStatus: 'ready', pageSpec: parsed, pageError: undefined};
                    } catch (error) {
                        return {
                            ...tab,
                            pageStatus: 'error',
                            pageError: error instanceof Error ? error.message : String(error),
                            pageSpec: fallbackPageSpec(tabToResult(tab)),
                        };
                    }
                }),
            );
        });

        const offPageError = EventsOn('page:error', (event: PageEvent) => {
            setTabs((currentTabs) =>
                currentTabs.map((tab) => {
                    if (tab.id !== event.tabId || tab.requestId !== event.requestId) {
                        return tab;
                    }
                    let fallback = fallbackPageSpec(tabToResult(tab));
                    if (event.spec) {
                        try {
                            fallback = normalizePageSpec(JSON.parse(event.spec), tabToResult(tab));
                        } catch {
                            // Keep local fallback.
                        }
                    }
                    return {
                        ...tab,
                        pageStatus: 'error',
                        pageError: event.message || 'Page generation failed.',
                        pageSpec: fallback,
                    };
                }),
            );
        });

        return () => {
            offStarted();
            offItem();
            offError();
            offDone();
            offPageStarted();
            offPageChunk();
            offPageDone();
            offPageError();
        };
    }, []);

    useEffect(() => {
        const closeMenu = () => setContextMenu(null);
        window.addEventListener('click', closeMenu);
        window.addEventListener('blur', closeMenu);
        return () => {
            window.removeEventListener('click', closeMenu);
            window.removeEventListener('blur', closeMenu);
        };
    }, []);

    function runCommand(nextQuery = command) {
        const trimmedQuery = nextQuery.trim();
        if (!trimmedQuery) {
            return;
        }
        setCommand(trimmedQuery);
        setContextMenu(null);
        startSearch(activeTabId, trimmedQuery);
    }

    function submitSearch(event: FormEvent<HTMLFormElement>) {
        event.preventDefault();
        runCommand();
    }

    return (
        <main className="app-shell">
            <section className="workspace">
                <div className="tab-strip">
                    <div className="tabs">
                        {tabs.map((tab) => (
                            <div
                                className={`tab ${tab.id === activeTabId ? 'active' : ''}`}
                                key={tab.id}
                                onClick={() => selectTab(tab)}
                                onKeyDown={(event) => handleTabKeyDown(event, tab)}
                                role="button"
                                tabIndex={0}
                            >
                                <span className={`tab-dot ${tab.kind}`}></span>
                                <span className="tab-title">{tab.title}</span>
                                <button
                                    className="tab-close"
                                    onClick={(event) => {
                                        event.stopPropagation();
                                        closeTab(tab.id);
                                    }}
                                    title="Close tab"
                                    type="button"
                                >
                                    <X size={12} strokeWidth={2.5} />
                                </button>
                            </div>
                        ))}
                    </div>
                    <button className="new-tab" onClick={addTab} title="New tab" type="button">
                        <Plus size={16} strokeWidth={2.5} />
                    </button>
                    <div className="window-controls">
                        <button onClick={WindowMinimise} title="Minimize" type="button">
                            <Minus size={15} strokeWidth={2.2} />
                        </button>
                        <button onClick={WindowToggleMaximise} title="Maximize" type="button">
                            <Square size={13} strokeWidth={2.1} />
                        </button>
                        <button className="close-window" onClick={Quit} title="Close" type="button">
                            <X size={15} strokeWidth={2.2} />
                        </button>
                    </div>
                </div>
                <div className={`progress-bar ${isActiveTabLoading ? 'active' : ''}`}>
                    <span></span>
                </div>

                <div className="navigation-row">
                    <button
                        className="back-button"
                        disabled={!canGoBack}
                        onClick={goBack}
                        title="Back"
                        type="button"
                    >
                        <ArrowLeft size={18} strokeWidth={2.4} />
                    </button>
                    <form className="command-bar" onSubmit={submitSearch}>
                        <span className="search-icon">
                            <Search size={18} strokeWidth={2} />
                        </span>
                        <input
                            autoFocus
                            onChange={(event) => setCommand(event.target.value)}
                            placeholder={activeTab.searchMode === 'sourced' ? 'Search the web and build a sourced page' : 'Search for a creative generated page'}
                            value={command}
                        />
                        <button type="submit">
                            Go <ArrowRight size={14} strokeWidth={2.5} />
                        </button>
                    </form>
                </div>

                <section className={`content ${hasIframe ? 'iframe-mode' : ''}`} onScroll={handleContentScroll}>
                    {!activeTab.query ? (
                        <div className="empty-state">
                            <div className="start-panel">
                                <div className="start-copy">
                                    <span>Search mode</span>
                                    <p>
                                        {activeTab.searchMode === 'sourced'
                                            ? 'Build a clean page from web sources, citations, and scraped context.'
                                            : 'Explore generated ideas first, then open one as a custom page.'}
                                    </p>
                                </div>
                                <div className="mode-toggle" role="group" aria-label="Search mode">
                                    <button
                                        className={activeTab.searchMode === 'sourced' ? 'active' : ''}
                                        onClick={() => setSearchMode('sourced')}
                                        type="button"
                                    >
                                        Sourced
                                    </button>
                                    <button
                                        className={activeTab.searchMode === 'creative' ? 'active' : ''}
                                        onClick={() => setSearchMode('creative')}
                                        type="button"
                                    >
                                        Creative
                                    </button>
                                </div>

                                <div className="start-copy" style={{ marginTop: '24px' }}>
                                    <span>Generation speed</span>
                                    <p>
                                        {activeTab.generationMode === 'superfast'
                                            ? 'Uses Cerebras AI API (GLM 4) for blazing fast generated pages.'
                                            : 'Uses OpenAI GPT models for high-quality results.'}
                                    </p>
                                </div>
                                <div className="mode-toggle" role="group" aria-label="Generation speed">
                                    <button
                                        className={activeTab.generationMode === 'standard' ? 'active' : ''}
                                        onClick={() => setGenerationMode('standard')}
                                        type="button"
                                    >
                                        Standard
                                    </button>
                                    <button
                                        className={activeTab.generationMode === 'superfast' ? 'active' : ''}
                                        onClick={() => setGenerationMode('superfast')}
                                        type="button"
                                    >
                                        Super Fast
                                    </button>
                                </div>
                            </div>
                        </div>
                    ) : activeTab.kind === 'generated' ? (
                        <GeneratedPageView tab={activeTab} />
                    ) : (
                        <div className="search-view">
                            <div>
                                <p className="eyebrow">
                                    {activeTab.suggestionStatus === 'loading' ? 'Dreaming' : 'Dream Search'}
                                </p>
                                <h1>{activeTab.query}</h1>
                                {activeTab.suggestionStatus === 'loading' && activeTab.suggestions.length === 0 ? (
                                    <p className="status-line">
                                        <Loader2 className="spin" size={15} /> Creating results...
                                    </p>
                                ) : null}
                                {activeTab.suggestionStatus === 'error' ? (
                                    <p className="error-line">{activeTab.suggestionError}</p>
                                ) : null}
                            </div>

                            {activeTab.suggestions.length > 0 ? (
                                <div className="result-list">
                                    {visibleResults.map((result) => (
                                        <article
                                            className="search-result"
                                            key={result.id}
                                            onContextMenu={(event) => handleResultContextMenu(event, result)}
                                            onMouseDown={(event) => handleResultMouseDown(event, result)}
                                        >
                                            <button
                                                className="result-main"
                                                onClick={() => openInSameTab(result)}
                                                type="button"
                                            >
                                                <span className="result-url">{result.url}</span>
                                                <h2>{result.title}</h2>
                                                <p>{result.description}</p>
                                            </button>
                                            <button
                                                className="result-open"
                                                onClick={() => openInSameTab(result)}
                                                title="Open"
                                                type="button"
                                            >
                                                <ExternalLink size={14} />
                                            </button>
                                        </article>
                                    ))}
                                </div>
                            ) : activeTab.suggestionStatus === 'done' ? (
                                <div className="empty-results">
                                    <p>No suggestions came back. Try a more specific search.</p>
                                </div>
                            ) : null}

                            {activeTab.suggestionStatus === 'loading' && activeTab.suggestions.length > 0 ? (
                                <p className="streaming-line">
                                    <Loader2 className="spin" size={14} /> More results are forming...
                                </p>
                            ) : null}
                        </div>
                    )}
                </section>
            </section>
            {contextMenu ? (
                <div
                    className="result-menu"
                    onClick={(event) => event.stopPropagation()}
                    style={{left: contextMenu.x, top: contextMenu.y}}
                >
                    <button onClick={() => openInSameTab(contextMenu.result)} type="button">
                        <ExternalLink size={14} /> Open in this tab
                    </button>
                    <button onClick={() => openInNewTab(contextMenu.result)} type="button">
                        <Plus size={14} /> Open in new tab
                    </button>
                    <button onClick={() => copyResult(contextMenu.result)} type="button">
                        <Copy size={14} /> Copy result
                    </button>
                </div>
            ) : null}
        </main>
    );
}

export default App;

function GeneratedPageView({tab}: {tab: Tab}) {
    if (tab.pageStatus === 'loading') {
        return (
            <div className="page-skeleton-container">
                <div className="generated-browser-bar">
                    <div className="browser-dots">
                        <span></span>
                        <span></span>
                        <span></span>
                    </div>
                    <div className="generated-url-bar">
                        <span className="lock-dot"></span>
                        <div className="skeleton skeleton-url"></div>
                    </div>
                    <span className="page-mode" style={{ color: 'var(--muted)', fontSize: '11px', fontWeight: 600 }}>
                        {tab.pageStream ? `${tab.pageStream.length.toLocaleString()} chars` : 'connecting...'}
                    </span>
                </div>
                <div className="page-skeleton-body">
                    <div className="skeleton skeleton-hero-title" style={{ display: 'flex', alignItems: 'center', paddingLeft: '16px', color: 'rgba(255,255,255,0.4)', fontSize: '12px', fontWeight: 600 }}>
                        Streaming components... {tab.pageStream ? `(${tab.pageStream.length.toLocaleString()} chars)` : ''}
                    </div>
                    <div className="skeleton skeleton-hero-desc"></div>
                    <div className="skeleton-grid">
                        <div className="skeleton skeleton-card"></div>
                        <div className="skeleton skeleton-card"></div>
                        <div className="skeleton skeleton-card"></div>
                        <div className="skeleton skeleton-card"></div>
                    </div>
                </div>
            </div>
        );
    }

    if (tab.pageStatus === 'error' && !tab.pageSpec) {
        return (
            <div className="page-loading error-line">
                {tab.pageError || 'Could not generate page.'}
            </div>
        );
    }

    if (!tab.pageSpec) {
        return null;
    }

    return <GeneratedPage spec={tab.pageSpec} isStreaming={false} />;
}

function GeneratedPage({spec, isStreaming}: {spec: PageSpec; isStreaming: boolean}) {
    const [runtime, setRuntime] = useState<Record<string, number | boolean | string | string[]>>({
        score: 0,
        focus: false,
        notes: [],
    });

    if (spec.customHtml) {
        return <CustomGeneratedFrame spec={spec} />;
    }

    function runAction(action: PageAction) {
        setRuntime((current) => {
            if (action.action === 'increment') {
                const currentValue = typeof current[action.target] === 'number' ? current[action.target] as number : 0;
                return {...current, [action.target]: currentValue + 1};
            }
            if (action.action === 'toggle') {
                return {...current, [action.target]: !(current[action.target] === true)};
            }
            if (action.action === 'append') {
                const currentList = Array.isArray(current[action.target]) ? current[action.target] as string[] : [];
                return {...current, [action.target]: [...currentList, `${action.label} ${currentList.length + 1}`]};
            }
            return {...current, [action.target]: 'highlighted'};
        });
    }

    const accent = spec.theme?.accent || '#8ab4f8';

    return (
        <div className="generated-page" style={{'--page-accent': accent} as CSSProperties}>
            <header className="generated-page-header">
                <span>{spec.theme?.mood || 'generated'}</span>
                <h1>{spec.title}</h1>
                {spec.subtitle ? <p>{spec.subtitle}</p> : null}
            </header>

            <div className="generated-sections">
                {spec.sections.map((section, index) => (
                    <GeneratedSection
                        key={`${section.type}-${section.title}-${index}`}
                        runtime={runtime}
                        runAction={runAction}
                        section={section}
                    />
                ))}
            </div>
        </div>
    );
}

function CustomGeneratedFrame({spec}: {spec: PageSpec}) {
    const srcDoc = `<!doctype html>
<html>
<head>
<meta charset="utf-8" />
<meta name="viewport" content="width=device-width, initial-scale=1" />
<style>
@import url('https://fonts.googleapis.com/css2?family=Inter:wght@400;500;600;700;800&display=swap');
:root{color-scheme:dark;--gb-accent:#8ab4f8;--gb-bg:#101114;--gb-panel:rgba(255,255,255,.07);--gb-line:rgba(255,255,255,.14);--gb-text:#f5f7fb;--gb-muted:#b7bec9}
html,body{margin:0;min-height:100%;background:var(--gb-bg);color:var(--gb-text);font-family:Inter,system-ui,-apple-system,Segoe UI,sans-serif;scrollbar-color:rgba(255,255,255,.35) transparent}
*{box-sizing:border-box}
body{min-height:100vh}
button,input,select,textarea{font:inherit}
button{min-height:40px;border:0;border-radius:12px;background:var(--gb-accent);color:#101114;padding:0 16px;font-weight:750;cursor:pointer;box-shadow:0 10px 24px rgba(0,0,0,.18);transition:transform .16s ease,filter .16s ease}
button:hover{filter:brightness(1.06);transform:translateY(-1px)}
button:active{transform:translateY(0)}
input,select,textarea{border:1px solid var(--gb-line);border-radius:12px;background:rgba(255,255,255,.08);color:var(--gb-text);padding:11px 12px;outline:none}
a{color:var(--gb-accent)}
canvas{max-width:100%}
main,section{max-width:100%}
${spec.customCss || ''}
</style>
</head>
<body>
${spec.customHtml || ''}
<script>
try {
${spec.customJs || ''}
} catch (error) {
  document.body.insertAdjacentHTML('beforeend', '<pre style="position:fixed;left:12px;right:12px;bottom:12px;white-space:pre-wrap;color:#ff8a8a;background:#1d1f24;border:1px solid rgba(255,255,255,.16);border-radius:12px;padding:12px;font:12px ui-monospace,monospace;z-index:9999"></pre>');
  document.body.lastElementChild.textContent = 'Generated script error: ' + error.message;
}
</script>
</body>
</html>`;

    return (
        <div className="custom-page-shell">
            <div className="generated-browser-bar">
                <div className="browser-dots">
                    <span></span>
                    <span></span>
                    <span></span>
                </div>
                <div className="generated-url-bar">
                    <span className="lock-dot"></span>
                    <strong>{spec.sourceUrl || generatedPageUrl({title: spec.title, query: spec.title})}</strong>
                </div>
                <span className="page-mode">{spec.mode || 'mini app'}</span>
            </div>
            <iframe
                className="custom-page-frame"
                sandbox="allow-scripts"
                srcDoc={srcDoc}
                title={spec.title}
            />
        </div>
    );
}

function GeneratedSection({
    section,
    runtime,
    runAction,
}: {
    section: PageSection;
    runtime: Record<string, number | boolean | string | string[]>;
    runAction: (action: PageAction) => void;
}) {
    return (
        <section className={`generated-section section-${section.type}`}>
            {section.title ? <h2>{section.title}</h2> : null}
            {section.description ? <p className="section-description">{section.description}</p> : null}

            {section.type === 'stats' ? (
                <div className="generated-stats">
                    {(section.items || []).map((item, index) => (
                        <div key={`${item.label}-${index}`}>
                            <span>{item.label}</span>
                            <strong>{item.value}</strong>
                        </div>
                    ))}
                </div>
            ) : null}

            {section.type === 'cards' || section.type === 'hero' ? (
                <div className="generated-cards">
                    {(section.items || []).map((item, index) => (
                        <article key={`${item.title}-${index}`}>
                            <h3>{item.title}</h3>
                            <p>{item.description}</p>
                        </article>
                    ))}
                </div>
            ) : null}

            {section.type === 'list' ? (
                <ol className="generated-list">
                    {(section.items || []).map((item, index) => (
                        <li key={`${item.title}-${index}`}>
                            <strong>{item.title}</strong>
                            <span>{item.description}</span>
                        </li>
                    ))}
                </ol>
            ) : null}

            {section.type === 'table' ? (
                <div className="generated-table">
                    {(section.items || []).map((item, index) => (
                        <div key={`${item.title}-${index}`}>
                            <span>{item.title || item.label}</span>
                            <span>{item.value || item.description}</span>
                        </div>
                    ))}
                </div>
            ) : null}

            {section.type === 'form' ? (
                <form className="generated-form" onSubmit={(event) => event.preventDefault()}>
                    {(section.fields || []).map((field) => (
                        <label key={field.label}>
                            <span>{field.label}</span>
                            <input placeholder={field.placeholder || field.label} />
                        </label>
                    ))}
                </form>
            ) : null}

            {section.actions && section.actions.length > 0 ? (
                <div className="generated-actions">
                    {section.actions.map((action) => (
                        <button key={`${action.label}-${action.target}`} onClick={() => runAction(action)} type="button">
                            {action.label}
                        </button>
                    ))}
                </div>
            ) : null}

            {section.type === 'controls' ? (
                <div className="runtime-panel">
                    {Object.entries(runtime).map(([key, value]) => (
                        <div key={key}>
                            <span>={key}</span>
                            <strong>{Array.isArray(value) ? value.length : String(value)}</strong>
                        </div>
                    ))}
                </div>
            ) : null}
        </section>
    );
}
