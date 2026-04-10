'use client'

import { Suspense, useEffect, useRef, useState, useCallback } from 'react'
import { useRouter, useSearchParams } from 'next/navigation'
import { Search } from 'lucide-react'
import { api } from '@/lib/api'
import { NODE_COLORS } from '@/lib/colors'
import { Badge } from '@/components/ui/badge'
import { Input } from '@/components/ui/input'
import type { NodeKind } from '@/lib/types'

interface SearchResult {
  kind: string
  name: string
  file: string
  line: number
  id: string
}

function parseCompactResults(text: string): SearchResult[] {
  if (!text.trim()) return []
  const lines = text.trim().split('\n')
  const results: SearchResult[] = []
  for (const line of lines) {
    // Format: "kind name file:line"
    const match = line.match(/^(\S+)\s+(\S+)\s+(.+):(\d+)$/)
    if (match) {
      const [, kind, name, file, lineStr] = match
      results.push({
        kind,
        name,
        file,
        line: parseInt(lineStr, 10),
        // Build symbol ID: file_path::name for navigation
        id: `${file}::${name}`,
      })
    }
  }
  return results
}

function groupByKind(results: SearchResult[]): Record<string, SearchResult[]> {
  const groups: Record<string, SearchResult[]> = {}
  for (const r of results) {
    if (!groups[r.kind]) groups[r.kind] = []
    groups[r.kind].push(r)
  }
  return groups
}

export default function SearchPage() {
  return (
    <Suspense
      fallback={
        <div className="flex items-center gap-2 py-12 text-zinc-500">
          <div className="h-5 w-5 animate-spin rounded-full border-2 border-zinc-700 border-t-zinc-400" />
          Loading search...
        </div>
      }
    >
      <SearchPageInner />
    </Suspense>
  )
}

function SearchPageInner() {
  const router = useRouter()
  const searchParams = useSearchParams()
  const initialQuery = searchParams.get('q') || ''

  const [query, setQuery] = useState(initialQuery)
  const [results, setResults] = useState<SearchResult[]>([])
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [searchTime, setSearchTime] = useState<number | null>(null)
  const [hasSearched, setHasSearched] = useState(false)

  const inputRef = useRef<HTMLInputElement>(null)
  const debounceRef = useRef<ReturnType<typeof setTimeout> | null>(null)

  const doSearch = useCallback(async (q: string) => {
    if (!q.trim()) {
      setResults([])
      setHasSearched(false)
      setSearchTime(null)
      return
    }

    setLoading(true)
    setError(null)
    const start = performance.now()

    try {
      const text = await api.searchSymbols(q, 50)
      const elapsed = performance.now() - start
      setResults(parseCompactResults(text))
      setSearchTime(elapsed)
      setHasSearched(true)
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Search failed')
      setResults([])
    } finally {
      setLoading(false)
    }
  }, [])

  // Auto-focus on mount
  useEffect(() => {
    inputRef.current?.focus()
  }, [])

  // Search from URL param on mount
  useEffect(() => {
    if (initialQuery) {
      doSearch(initialQuery)
    }
  }, [initialQuery, doSearch])

  const handleInputChange = (e: React.ChangeEvent<HTMLInputElement>) => {
    const value = e.target.value
    setQuery(value)

    // Update URL without navigation
    const url = value ? `?q=${encodeURIComponent(value)}` : '/search'
    window.history.replaceState(null, '', url)

    // Debounce search
    if (debounceRef.current) clearTimeout(debounceRef.current)
    debounceRef.current = setTimeout(() => {
      doSearch(value)
    }, 300)
  }

  const handleSubmit = (e: React.FormEvent) => {
    e.preventDefault()
    if (debounceRef.current) clearTimeout(debounceRef.current)
    doSearch(query)
  }

  const handleResultClick = (result: SearchResult) => {
    const encodedId = encodeURIComponent(result.id)
    router.push(`/symbol/${encodedId}`)
  }

  const grouped = groupByKind(results)
  const kindOrder = ['function', 'method', 'type', 'interface', 'variable', 'file', 'package', 'import']
  const sortedKinds = Object.keys(grouped).sort(
    (a, b) => (kindOrder.indexOf(a) === -1 ? 99 : kindOrder.indexOf(a)) -
              (kindOrder.indexOf(b) === -1 ? 99 : kindOrder.indexOf(b))
  )

  return (
    <div className="space-y-6">
      <div>
        <h1 className="text-xl font-semibold text-zinc-100">Search Symbols</h1>
        <p className="text-sm text-zinc-500">
          Find functions, types, methods, and more across the codebase
        </p>
      </div>

      <form onSubmit={handleSubmit} className="flex items-center gap-3">
        <div className="relative flex-1">
          <Search className="absolute left-3 top-1/2 h-4 w-4 -translate-y-1/2 text-zinc-500" />
          <Input
            ref={inputRef}
            type="text"
            placeholder="Search symbols... (e.g. handleRequest, NodeKind, parseFile)"
            value={query}
            onChange={handleInputChange}
            className="h-10 pl-10 bg-zinc-900 border-zinc-800 text-zinc-100 placeholder:text-zinc-600"
          />
        </div>
      </form>

      {/* Status bar */}
      {hasSearched && !error && (
        <div className="flex items-center gap-3 text-xs text-zinc-500">
          <span>
            {results.length} result{results.length !== 1 ? 's' : ''}
          </span>
          {searchTime !== null && (
            <span>{searchTime.toFixed(0)}ms</span>
          )}
        </div>
      )}

      {/* Error state */}
      {error && (
        <div className="rounded-lg border border-red-900/50 bg-red-950/30 p-4 text-sm text-red-400">
          {error}
        </div>
      )}

      {/* Loading state */}
      {loading && (
        <div className="flex items-center gap-2 text-sm text-zinc-500">
          <div className="h-4 w-4 animate-spin rounded-full border-2 border-zinc-700 border-t-zinc-400" />
          Searching...
        </div>
      )}

      {/* Empty state */}
      {hasSearched && !loading && results.length === 0 && !error && (
        <div className="py-12 text-center">
          <p className="text-zinc-500">No symbols found for &quot;{query}&quot;</p>
          <p className="mt-1 text-xs text-zinc-600">
            Try a different query or check the spelling
          </p>
        </div>
      )}

      {/* Results grouped by kind */}
      {!loading && sortedKinds.map((kind) => (
        <div key={kind} className="space-y-2">
          <h2 className="flex items-center gap-2 text-sm font-medium text-zinc-400">
            <span
              className="inline-block h-2.5 w-2.5 rounded-full"
              style={{ backgroundColor: NODE_COLORS[kind as NodeKind] || '#6b7280' }}
            />
            {kind.charAt(0).toUpperCase() + kind.slice(1)}s
            <span className="text-zinc-600">({grouped[kind].length})</span>
          </h2>
          <div className="space-y-1">
            {grouped[kind].map((result, i) => (
              <button
                key={`${result.id}-${i}`}
                onClick={() => handleResultClick(result)}
                className="flex w-full items-center gap-3 rounded-lg border border-zinc-800/50 bg-zinc-900/50 px-4 py-2.5 text-left transition-colors hover:border-zinc-700 hover:bg-zinc-900"
              >
                <Badge
                  variant="secondary"
                  className="shrink-0 font-mono text-[10px]"
                  style={{
                    backgroundColor: `${NODE_COLORS[result.kind as NodeKind] || '#6b7280'}20`,
                    color: NODE_COLORS[result.kind as NodeKind] || '#6b7280',
                    borderColor: `${NODE_COLORS[result.kind as NodeKind] || '#6b7280'}30`,
                  }}
                >
                  {result.kind}
                </Badge>
                <span className="font-mono text-sm font-medium text-zinc-200">
                  {result.name}
                </span>
                <span className="ml-auto truncate text-xs text-zinc-600">
                  {result.file}:{result.line}
                </span>
              </button>
            ))}
          </div>
        </div>
      ))}
    </div>
  )
}
