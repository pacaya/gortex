'use client'

import { useEffect, useRef, useState } from 'react'
import dynamic from 'next/dynamic'
import { api } from '@/lib/api'
import { useStore } from '@/lib/store'
import type { GraphData, SubGraph } from '@/lib/types'
import GraphFilters from '@/components/graph/GraphFilters'
import NodeDetail from '@/components/graph/NodeDetail'
import { Loader2 } from 'lucide-react'

const GraphCanvas = dynamic(() => import('@/components/graph/GraphCanvas'), {
  ssr: false,
  loading: () => (
    <div className="flex h-full w-full items-center justify-center">
      <Loader2 className="h-6 w-6 animate-spin text-zinc-500" />
    </div>
  ),
})

export default function GraphExplorerPage() {
  const [graphData, setGraphData] = useState<GraphData | null>(null)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)

  const { selectedNodeId } = useStore()

  const fitCameraRef = useRef<(() => void) | null>(null)
  const relayoutRef = useRef<(() => void) | null>(null)

  useEffect(() => {
    let mounted = true

    async function fetchGraph() {
      try {
        setLoading(true)
        // Check graph size first — don't load 50k+ nodes into the browser
        const stats = await api.stats()
        if (stats.total_nodes > 10000) {
          // For large graphs, load only non-file, non-import, non-variable nodes
          // by fetching graph and filtering client-side
          const data = await api.getGraph()
          if (!mounted) return
          const filteredNodes = data.nodes.filter(n =>
            n.kind !== 'file' && n.kind !== 'import' && n.kind !== 'variable'
          )
          const nodeIds = new Set(filteredNodes.map(n => n.id))
          const filteredEdges = data.edges.filter(e =>
            nodeIds.has(e.from) && nodeIds.has(e.to)
          )
          setGraphData({
            nodes: filteredNodes,
            edges: filteredEdges,
            stats: data.stats,
          })
        } else {
          const data = await api.getGraph()
          if (!mounted) return
          setGraphData(data)
        }
        setError(null)
      } catch (err) {
        if (!mounted) return
        setError(err instanceof Error ? err.message : 'Failed to load graph')
      } finally {
        if (mounted) setLoading(false)
      }
    }

    fetchGraph()
    return () => { mounted = false }
  }, [])

  function handleFitCamera() {
    fitCameraRef.current?.()
  }

  function handleRelayout() {
    relayoutRef.current?.()
  }

  function handleFocusCluster(_cluster: SubGraph) {
    // Future: replace graph data with cluster subgraph for focused view
  }

  if (loading) {
    return (
      <div className="flex h-full items-center justify-center">
        <div className="flex items-center gap-3 text-zinc-500">
          <Loader2 className="h-5 w-5 animate-spin" />
          <span className="text-sm">Loading graph data...</span>
        </div>
      </div>
    )
  }

  if (error) {
    return (
      <div className="flex h-full items-center justify-center">
        <div className="max-w-sm rounded-lg border border-zinc-800 bg-zinc-900 p-6 text-center">
          <p className="mb-2 text-sm font-medium text-red-400">Failed to load graph</p>
          <p className="text-xs text-zinc-500">{error}</p>
        </div>
      </div>
    )
  }

  if (!graphData || graphData.nodes.length === 0) {
    return (
      <div className="flex h-full items-center justify-center">
        <p className="text-sm text-zinc-500">No graph data available. Make sure a repository is indexed.</p>
      </div>
    )
  }

  return (
    <div className="-m-6 flex h-[calc(100vh-theme(spacing.24))] overflow-hidden">
      <GraphFilters
        nodeCount={graphData.nodes.length}
        edgeCount={graphData.edges.length}
        onFitCamera={handleFitCamera}
        onRelayout={handleRelayout}
      />

      <div className="relative flex-1 overflow-hidden bg-zinc-950">
        <GraphCanvas
          nodes={graphData.nodes}
          edges={graphData.edges}
          fitCameraRef={fitCameraRef}
          relayoutRef={relayoutRef}
        />

        {/* Zoom controls overlay */}
        <div className="absolute bottom-4 right-4 flex flex-col gap-1">
          <button
            onClick={handleFitCamera}
            className="flex h-8 w-8 items-center justify-center rounded-lg border border-zinc-700 bg-zinc-900/90 text-xs font-medium text-zinc-300 backdrop-blur hover:bg-zinc-800"
            title="Fit to screen"
          >
            <svg className="h-4 w-4" fill="none" viewBox="0 0 24 24" stroke="currentColor" strokeWidth={2}>
              <path strokeLinecap="round" strokeLinejoin="round" d="M4 8V4m0 0h4M4 4l5 5m11-1V4m0 0h-4m4 0l-5 5M4 16v4m0 0h4m-4 0l5-5m11 5v-4m0 4h-4m4 0l-5-5" />
            </svg>
          </button>
          <button
            onClick={() => {
              // Zoom in by decreasing camera ratio
              const canvas = document.querySelector('[data-sigma-mouse-container]') as HTMLElement | null
              if (canvas) {
                const event = new WheelEvent('wheel', { deltaY: -100, bubbles: true })
                canvas.dispatchEvent(event)
              }
            }}
            className="flex h-8 w-8 items-center justify-center rounded-lg border border-zinc-700 bg-zinc-900/90 text-lg font-medium text-zinc-300 backdrop-blur hover:bg-zinc-800"
            title="Zoom in"
          >
            +
          </button>
          <button
            onClick={() => {
              const canvas = document.querySelector('[data-sigma-mouse-container]') as HTMLElement | null
              if (canvas) {
                const event = new WheelEvent('wheel', { deltaY: 100, bubbles: true })
                canvas.dispatchEvent(event)
              }
            }}
            className="flex h-8 w-8 items-center justify-center rounded-lg border border-zinc-700 bg-zinc-900/90 text-lg font-medium text-zinc-300 backdrop-blur hover:bg-zinc-800"
            title="Zoom out"
          >
            -
          </button>
        </div>
      </div>

      {selectedNodeId && <NodeDetail onFocusCluster={handleFocusCluster} />}
    </div>
  )
}
