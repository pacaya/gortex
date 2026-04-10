'use client'

import { useEffect, useState, useCallback, use } from 'react'
import Link from 'next/link'
import { ArrowLeft, FileCode, MapPin } from 'lucide-react'
import { api } from '@/lib/api'
import { NODE_COLORS, LANGUAGE_COLORS } from '@/lib/colors'
import { Badge } from '@/components/ui/badge'
import { Tabs, TabsList, TabsTrigger, TabsContent } from '@/components/ui/tabs'
import { SourceView } from '@/components/symbol/SourceView'
import type { GortexNode, NodeKind, SubGraph } from '@/lib/types'

interface TabData {
  loaded: boolean
  loading: boolean
  error: string | null
  nodes: GortexNode[]
}

const INITIAL_TAB_DATA: TabData = {
  loaded: false,
  loading: false,
  error: null,
  nodes: [],
}

function NodeList({ nodes }: { nodes: GortexNode[] }) {
  if (nodes.length === 0) {
    return <p className="py-4 text-center text-sm text-zinc-600">None found</p>
  }

  return (
    <div className="space-y-1">
      {nodes.map((node) => (
        <Link
          key={node.id}
          href={`/symbol/${encodeURIComponent(node.id)}`}
          className="flex items-center gap-3 rounded-lg border border-zinc-800/50 bg-zinc-900/50 px-4 py-2.5 transition-colors hover:border-zinc-700 hover:bg-zinc-900"
        >
          <Badge
            variant="secondary"
            className="shrink-0 font-mono text-[10px]"
            style={{
              backgroundColor: `${NODE_COLORS[node.kind] || '#6b7280'}20`,
              color: NODE_COLORS[node.kind] || '#6b7280',
              borderColor: `${NODE_COLORS[node.kind] || '#6b7280'}30`,
            }}
          >
            {node.kind}
          </Badge>
          <span className="font-mono text-sm font-medium text-zinc-200">
            {node.name}
          </span>
          <span className="ml-auto truncate text-xs text-zinc-600">
            {node.file_path}:{node.start_line}
          </span>
        </Link>
      ))}
    </div>
  )
}

function TabLoadingState() {
  return (
    <div className="flex items-center gap-2 py-4 text-sm text-zinc-500">
      <div className="h-4 w-4 animate-spin rounded-full border-2 border-zinc-700 border-t-zinc-400" />
      Loading...
    </div>
  )
}

function TabErrorState({ error }: { error: string }) {
  return (
    <div className="rounded-lg border border-red-900/50 bg-red-950/30 p-3 text-sm text-red-400">
      {error}
    </div>
  )
}

export default function SymbolDetailPage({
  params,
}: {
  params: Promise<{ id: string }>
}) {
  const { id: rawId } = use(params)
  const id = decodeURIComponent(rawId)

  const [symbol, setSymbol] = useState<GortexNode | null>(null)
  const [source, setSource] = useState<string | null>(null)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)

  const [callers, setCallers] = useState<TabData>(INITIAL_TAB_DATA)
  const [callees, setCallees] = useState<TabData>(INITIAL_TAB_DATA)
  const [usages, setUsages] = useState<TabData>(INITIAL_TAB_DATA)
  const [dependencies, setDependencies] = useState<TabData>(INITIAL_TAB_DATA)
  const [dependents, setDependents] = useState<TabData>(INITIAL_TAB_DATA)

  const isUnresolved = id.startsWith('unresolved::')

  // Fetch symbol info and source
  useEffect(() => {
    let mounted = true
    setLoading(true)
    setError(null)

    if (isUnresolved) {
      setError(`"${id.replace('unresolved::', '')}" is an external dependency — it is not indexed in the graph.`)
      setLoading(false)
      return
    }

    Promise.all([api.getSymbol(id), api.getSymbolSource(id)])
      .then(([sym, src]) => {
        if (!mounted) return
        if (!sym) {
          setError('Symbol not found')
        } else {
          setSymbol(sym)
          setSource(src)
        }
      })
      .catch((err) => {
        if (!mounted) return
        setError(err instanceof Error ? err.message : 'Failed to load symbol')
      })
      .finally(() => {
        if (mounted) setLoading(false)
      })

    return () => { mounted = false }
  }, [id])

  const loadTab = useCallback(
    async (
      tabName: string,
      setter: React.Dispatch<React.SetStateAction<TabData>>,
      fetcher: () => Promise<SubGraph>,
    ) => {
      setter((prev) => {
        if (prev.loaded || prev.loading) return prev
        return { ...prev, loading: true }
      })

      try {
        const data = await fetcher()
        setter({
          loaded: true,
          loading: false,
          error: null,
          nodes: data.nodes || [],
        })
      } catch (err) {
        setter({
          loaded: true,
          loading: false,
          error: err instanceof Error ? err.message : `Failed to load ${tabName}`,
          nodes: [],
        })
      }
    },
    [],
  )

  const handleTabChange = (value: unknown) => {
    const tab = String(value)
    switch (tab) {
      case 'callers':
        if (!callers.loaded && !callers.loading)
          loadTab('callers', setCallers, () => api.getCallers(id))
        break
      case 'callees':
        if (!callees.loaded && !callees.loading)
          loadTab('callees', setCallees, () => api.getCallChain(id))
        break
      case 'usages':
        if (!usages.loaded && !usages.loading)
          loadTab('usages', setUsages, () => api.findUsages(id))
        break
      case 'dependencies':
        if (!dependencies.loaded && !dependencies.loading)
          loadTab('dependencies', setDependencies, () => api.getDependencies(id))
        break
      case 'dependents':
        if (!dependents.loaded && !dependents.loading)
          loadTab('dependents', setDependents, () => api.getDependents(id))
        break
    }
  }

  if (loading) {
    return (
      <div className="flex items-center gap-2 py-12 text-zinc-500">
        <div className="h-5 w-5 animate-spin rounded-full border-2 border-zinc-700 border-t-zinc-400" />
        Loading symbol...
      </div>
    )
  }

  if (error || !symbol) {
    return (
      <div className="space-y-4">
        <Link
          href="/search"
          className="inline-flex items-center gap-1 text-sm text-zinc-500 hover:text-zinc-300"
        >
          <ArrowLeft className="h-4 w-4" />
          Back to search
        </Link>
        <div className="rounded-lg border border-red-900/50 bg-red-950/30 p-4 text-sm text-red-400">
          {error || 'Symbol not found'}
        </div>
      </div>
    )
  }

  const kindColor = NODE_COLORS[symbol.kind] || '#6b7280'
  const langColor = LANGUAGE_COLORS[symbol.language] || '#6b7280'

  return (
    <div className="space-y-6">
      {/* Navigation */}
      <Link
        href="/search"
        className="inline-flex items-center gap-1 text-sm text-zinc-500 hover:text-zinc-300"
      >
        <ArrowLeft className="h-4 w-4" />
        Back to search
      </Link>

      {/* Header */}
      <div className="space-y-3">
        <div className="flex flex-wrap items-center gap-3">
          <h1 className="font-mono text-xl font-semibold text-zinc-100">
            {symbol.name}
          </h1>
          <Badge
            variant="secondary"
            className="font-mono text-xs"
            style={{
              backgroundColor: `${kindColor}20`,
              color: kindColor,
              borderColor: `${kindColor}30`,
            }}
          >
            {symbol.kind}
          </Badge>
          <Badge
            variant="secondary"
            className="font-mono text-xs"
            style={{
              backgroundColor: `${langColor}20`,
              color: langColor,
              borderColor: `${langColor}30`,
            }}
          >
            {symbol.language}
          </Badge>
        </div>
        <div className="flex items-center gap-4 text-sm text-zinc-500">
          <span className="flex items-center gap-1">
            <FileCode className="h-3.5 w-3.5" />
            {symbol.file_path}
          </span>
          <span className="flex items-center gap-1">
            <MapPin className="h-3.5 w-3.5" />
            Line {symbol.start_line}
            {symbol.end_line > symbol.start_line && `\u2013${symbol.end_line}`}
          </span>
        </div>
        {symbol.qual_name && symbol.qual_name !== symbol.name && (
          <p className="font-mono text-xs text-zinc-600">{symbol.qual_name}</p>
        )}
      </div>

      {/* Source code */}
      {source && (
        <SourceView
          source={source}
          startLine={symbol.start_line}
          language={symbol.language}
        />
      )}

      {/* Relationship tabs */}
      <Tabs defaultValue="callers" onValueChange={handleTabChange}>
        <TabsList variant="line">
          <TabsTrigger value="callers">
            Callers{callers.loaded ? ` (${callers.nodes.length})` : ''}
          </TabsTrigger>
          <TabsTrigger value="callees">
            Callees{callees.loaded ? ` (${callees.nodes.length})` : ''}
          </TabsTrigger>
          <TabsTrigger value="usages">
            Usages{usages.loaded ? ` (${usages.nodes.length})` : ''}
          </TabsTrigger>
          <TabsTrigger value="dependencies">
            Dependencies{dependencies.loaded ? ` (${dependencies.nodes.length})` : ''}
          </TabsTrigger>
          <TabsTrigger value="dependents">
            Dependents{dependents.loaded ? ` (${dependents.nodes.length})` : ''}
          </TabsTrigger>
        </TabsList>

        <TabsContent value="callers" className="mt-4">
          {callers.loading && <TabLoadingState />}
          {callers.error && <TabErrorState error={callers.error} />}
          {callers.loaded && !callers.error && <NodeList nodes={callers.nodes} />}
          {!callers.loaded && !callers.loading && (
            <p className="py-4 text-center text-sm text-zinc-600">
              Click to load callers
            </p>
          )}
        </TabsContent>

        <TabsContent value="callees" className="mt-4">
          {callees.loading && <TabLoadingState />}
          {callees.error && <TabErrorState error={callees.error} />}
          {callees.loaded && !callees.error && <NodeList nodes={callees.nodes} />}
          {!callees.loaded && !callees.loading && (
            <p className="py-4 text-center text-sm text-zinc-600">
              Click to load callees
            </p>
          )}
        </TabsContent>

        <TabsContent value="usages" className="mt-4">
          {usages.loading && <TabLoadingState />}
          {usages.error && <TabErrorState error={usages.error} />}
          {usages.loaded && !usages.error && <NodeList nodes={usages.nodes} />}
          {!usages.loaded && !usages.loading && (
            <p className="py-4 text-center text-sm text-zinc-600">
              Click to load usages
            </p>
          )}
        </TabsContent>

        <TabsContent value="dependencies" className="mt-4">
          {dependencies.loading && <TabLoadingState />}
          {dependencies.error && <TabErrorState error={dependencies.error} />}
          {dependencies.loaded && !dependencies.error && <NodeList nodes={dependencies.nodes} />}
          {!dependencies.loaded && !dependencies.loading && (
            <p className="py-4 text-center text-sm text-zinc-600">
              Click to load dependencies
            </p>
          )}
        </TabsContent>

        <TabsContent value="dependents" className="mt-4">
          {dependents.loading && <TabLoadingState />}
          {dependents.error && <TabErrorState error={dependents.error} />}
          {dependents.loaded && !dependents.error && <NodeList nodes={dependents.nodes} />}
          {!dependents.loaded && !dependents.loading && (
            <p className="py-4 text-center text-sm text-zinc-600">
              Click to load dependents
            </p>
          )}
        </TabsContent>
      </Tabs>
    </div>
  )
}
