'use client'

import { useState } from 'react'
import { useStore } from '@/lib/store'
import { NODE_COLORS } from '@/lib/colors'
import { api } from '@/lib/api'
import type { NodeKind, SubGraph } from '@/lib/types'
import { Button } from '@/components/ui/button'
import { Badge } from '@/components/ui/badge'
import { X, Code2, Share2, Loader2 } from 'lucide-react'

interface NodeDetailProps {
  onFocusCluster?: (subgraph: SubGraph) => void
}

export default function NodeDetail({ onFocusCluster }: NodeDetailProps) {
  const { selectedNode, selectedNodeId, selectNode } = useStore()
  const [loadingCluster, setLoadingCluster] = useState(false)

  if (!selectedNode || !selectedNodeId) return null

  const color = NODE_COLORS[selectedNode.kind as NodeKind] || '#6b7280'
  const signature = selectedNode.meta?.signature as string | undefined
  const callers = (selectedNode.meta?.callers as string[] | undefined) ?? []
  const callees = (selectedNode.meta?.callees as string[] | undefined) ?? []

  async function handleShowCluster() {
    if (!selectedNodeId) return
    setLoadingCluster(true)
    try {
      const cluster = await api.getCluster(selectedNodeId, 2)
      onFocusCluster?.(cluster)
    } catch {
      // ignore errors
    } finally {
      setLoadingCluster(false)
    }
  }

  return (
    <div className="flex h-full w-[300px] shrink-0 flex-col border-l border-zinc-800 bg-zinc-900/50 overflow-y-auto">
      {/* Header */}
      <div className="flex items-center justify-between border-b border-zinc-800 px-4 py-3">
        <h2 className="text-sm font-semibold text-zinc-300">Node Detail</h2>
        <button
          onClick={() => selectNode(null, null)}
          className="rounded p-1 text-zinc-500 hover:bg-zinc-800 hover:text-zinc-300"
        >
          <X className="h-4 w-4" />
        </button>
      </div>

      <div className="space-y-4 p-4">
        {/* Name and kind */}
        <div>
          <h3 className="text-base font-medium text-zinc-100 break-all">{selectedNode.name}</h3>
          <Badge
            variant="secondary"
            className="mt-1.5 border text-xs"
            style={{
              backgroundColor: color + '1a',
              borderColor: color + '40',
              color: color,
            }}
          >
            {selectedNode.kind}
          </Badge>
        </div>

        {/* File info */}
        <div className="space-y-1.5 text-xs">
          <div className="flex items-start gap-2">
            <span className="shrink-0 text-zinc-500">File:</span>
            <span className="text-zinc-300 break-all font-mono">{selectedNode.file_path}</span>
          </div>
          <div className="flex items-center gap-2">
            <span className="text-zinc-500">Lines:</span>
            <span className="text-zinc-300 font-mono">{selectedNode.start_line}--{selectedNode.end_line}</span>
          </div>
          <div className="flex items-center gap-2">
            <span className="text-zinc-500">Language:</span>
            <span className="text-zinc-300">{selectedNode.language}</span>
          </div>
        </div>

        {/* Signature */}
        {signature && (
          <div>
            <p className="mb-1 text-xs font-medium uppercase tracking-wider text-zinc-500">Signature</p>
            <pre className="rounded bg-zinc-800/80 p-2 text-xs text-zinc-300 overflow-x-auto font-mono">
              {signature}
            </pre>
          </div>
        )}

        {/* Actions */}
        <div className="flex gap-2">
          <a href={`/symbol/${encodeURIComponent(selectedNodeId)}`}>
            <Button variant="outline" size="sm" className="gap-1.5">
              <Code2 className="h-3.5 w-3.5" />
              View Source
            </Button>
          </a>
          <Button
            variant="outline"
            size="sm"
            className="gap-1.5"
            onClick={handleShowCluster}
            disabled={loadingCluster}
          >
            {loadingCluster ? (
              <Loader2 className="h-3.5 w-3.5 animate-spin" />
            ) : (
              <Share2 className="h-3.5 w-3.5" />
            )}
            Show Cluster
          </Button>
        </div>

        {/* Callers */}
        {callers.length > 0 && (
          <div>
            <p className="mb-1.5 text-xs font-medium uppercase tracking-wider text-zinc-500">
              Callers ({callers.length})
            </p>
            <ul className="space-y-0.5">
              {callers.map((id) => (
                <li key={id}>
                  <button
                    onClick={() => selectNode(id)}
                    className="w-full truncate rounded px-1.5 py-0.5 text-left text-xs font-mono text-blue-400 hover:bg-zinc-800"
                  >
                    {id}
                  </button>
                </li>
              ))}
            </ul>
          </div>
        )}

        {/* Callees */}
        {callees.length > 0 && (
          <div>
            <p className="mb-1.5 text-xs font-medium uppercase tracking-wider text-zinc-500">
              Callees ({callees.length})
            </p>
            <ul className="space-y-0.5">
              {callees.map((id) => (
                <li key={id}>
                  <button
                    onClick={() => selectNode(id)}
                    className="w-full truncate rounded px-1.5 py-0.5 text-left text-xs font-mono text-blue-400 hover:bg-zinc-800"
                  >
                    {id}
                  </button>
                </li>
              ))}
            </ul>
          </div>
        )}
      </div>
    </div>
  )
}
