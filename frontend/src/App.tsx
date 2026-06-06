import { FormEvent, useMemo, useState } from 'react';
import {
    Search,
    Plus,
    X,
    Minus,
    Square,
    ArrowRight,
    Cpu,
    GitBranch,
    FileCode,
    ExternalLink
} from 'lucide-react';
import './App.css';
import {Quit, WindowMinimise, WindowToggleMaximise} from '../wailsjs/runtime';

type TabKind = 'search' | 'generated';

type Tab = {
    id: number;
    title: string;
    query: string;
    kind: TabKind;
    createdAt: string;
};

const starterTabs: Tab[] = [
    {
        id: 1,
        title: 'New search',
        query: '',
        kind: 'search',
        createdAt: 'Just now',
    },
];

function classifyQuery(query: string): TabKind {
    const normalized = query.toLowerCase();
    const generateWords = ['create', 'generate', 'build', 'make', 'design'];
    return generateWords.some((word) => normalized.includes(word)) ? 'generated' : 'search';
}

function titleFromQuery(query: string) {
    if (!query.trim()) {
        return 'New search';
    }

    return query.trim().length > 22 ? `${query.trim().slice(0, 22)}...` : query.trim();
}

function App() {
    const [tabs, setTabs] = useState<Tab[]>(starterTabs);
    const [activeTabId, setActiveTabId] = useState(1);
    const [command, setCommand] = useState('');

    const activeTab = useMemo(
        () => tabs.find((tab) => tab.id === activeTabId) ?? tabs[0],
        [activeTabId, tabs],
    );

    function addTab() {
        const nextId = Date.now();
        const newTab: Tab = {
            id: nextId,
            title: 'New search',
            query: '',
            kind: 'search',
            createdAt: 'Just now',
        };

        setTabs((currentTabs) => [...currentTabs, newTab]);
        setActiveTabId(nextId);
        setCommand('');
    }

    function closeTab(tabId: number) {
        setTabs((currentTabs) => {
            if (currentTabs.length === 1) {
                return currentTabs;
            }

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
    }

    function runCommand(nextQuery = command) {
        const trimmedQuery = nextQuery.trim();
        if (!trimmedQuery) {
            return;
        }

        setTabs((currentTabs) =>
            currentTabs.map((tab) =>
                tab.id === activeTabId
                    ? {
                        ...tab,
                        title: titleFromQuery(trimmedQuery),
                        query: trimmedQuery,
                        kind: classifyQuery(trimmedQuery),
                    }
                    : tab,
            ),
        );
        setCommand(trimmedQuery);
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
                            <button
                                className={`tab ${tab.id === activeTabId ? 'active' : ''}`}
                                key={tab.id}
                                onClick={() => selectTab(tab)}
                                type="button"
                            >
                                <span className={`tab-dot ${tab.kind}`}></span>
                                <span className="tab-title">{tab.title}</span>
                                <span
                                    className="tab-close"
                                    onClick={(event) => {
                                        event.stopPropagation();
                                        closeTab(tab.id);
                                    }}
                                    title="Close tab"
                                >
                                    <X size={12} strokeWidth={2.5} />
                                </span>
                            </button>
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

                <form className="command-bar" onSubmit={submitSearch}>
                    <span className="search-icon">
                        <Search size={18} strokeWidth={2} />
                    </span>
                    <input
                        autoFocus
                        onChange={(event) => setCommand(event.target.value)}
                        placeholder="What app or website do you want to open?"
                        value={command}
                    />
                    <button type="submit">
                        Go <ArrowRight size={14} strokeWidth={2.5} />
                    </button>
                </form>

                <section className="content">
                    {!activeTab.query ? (
                        <div className="empty-state">
                            <p className="brand-name">Morph</p>
                            <h1>What should we open next?</h1>
                            <p>Search the web, open a website, or describe an app to create.</p>
                        </div>
                    ) : activeTab.kind === 'generated' ? (
                        <div className="generated-view">
                            <div>
                                <p className="eyebrow">Generated app draft</p>
                                <h1>{activeTab.query}</h1>
                                <p>
                                    This tab is ready to call the LLM endpoint and render a JSON-defined
                                    interface from trusted components.
                                </p>
                            </div>

                            <div className="preview-grid">
                                <article>
                                    <span>
                                        <GitBranch size={18} />
                                    </span>
                                    <h2>Intent router</h2>
                                    <p>Classifies the prompt as app generation, site navigation, or search.</p>
                                </article>
                                <article>
                                    <span>
                                        <Cpu size={18} />
                                    </span>
                                    <h2>Schema output</h2>
                                    <p>Receives screens, components, theme tokens, actions, and image requests.</p>
                                </article>
                                <article>
                                    <span>
                                        <FileCode size={18} />
                                    </span>
                                    <h2>Renderer</h2>
                                    <p>Maps validated JSON to reusable React components inside this tab.</p>
                                </article>
                            </div>
                        </div>
                    ) : (
                        <div className="search-view">
                            <p className="eyebrow">Search results</p>
                            <h1>{activeTab.query}</h1>
                            <div className="result-list">
                                {['Top result', 'Related app', 'Web reference'].map((label, index) => (
                                    <article key={label}>
                                        <div>
                                            <span>{label}</span>
                                            <h2>{activeTab.query}</h2>
                                            <p>
                                                Placeholder result {index + 1}. Next we can connect this to a real
                                                search API or route URLs into an embedded browser view.
                                            </p>
                                        </div>
                                        <button type="button">
                                            Open <ExternalLink size={12} />
                                        </button>
                                    </article>
                                ))}
                            </div>
                        </div>
                    )}
                </section>
            </section>
        </main>
    );
}

export default App;
