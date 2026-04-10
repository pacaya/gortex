'use client'

import { useEffect, useRef, useCallback } from 'react'
import Graph from 'graphology'
import Sigma from 'sigma'
import { EdgeArrowProgram } from 'sigma/rendering'
import FA2LayoutSupervisor from 'graphology-layout-forceatlas2/worker'
import { inferSettings } from 'graphology-layout-forceatlas2'
import type { GortexNode, GortexEdge, NodeKind, EdgeKind } from '@/lib/types'
import { NODE_COLORS, EDGE_COLORS } from '@/lib/colors'
import { useStore } from '@/lib/store'

interface GraphCanvasProps {
  nodes: GortexNode[]
  edges: GortexEdge[]
  fitCameraRef?: React.MutableRefObject<(() => void) | null>
  relayoutRef?: React.MutableRefObject<(() => void) | null>
}

export default function GraphCanvas({ nodes, edges, fitCameraRef, relayoutRef }: GraphCanvasProps) {
  const containerRef = useRef<HTMLDivElement>(null)
  const sigmaRef = useRef<Sigma | null>(null)
  const graphRef = useRef<Graph | null>(null)
  const layoutRef = useRef<FA2LayoutSupervisor | null>(null)
  const hoveredNodeRef = useRef<string | null>(null)

  const { visibleKinds, hideTestFiles, hideImports, selectNode, setHoveredNode } = useStore()

  // Build graph from data
  const buildGraph = useCallback(() => {
    const graph = new Graph({ multi: true, type: 'directed' })

    const nodeIds = new Set<string>()
    for (const node of nodes) {
      if (nodeIds.has(node.id)) continue
      nodeIds.add(node.id)

      // Random initial positions
      graph.addNode(node.id, {
        x: Math.random() * 100,
        y: Math.random() * 100,
        label: node.name,
        size: 5,
        color: NODE_COLORS[node.kind as NodeKind] || '#6b7280',
        nodeKind: node.kind,
        filePath: node.file_path,
        hidden: false,
      })
    }

    for (const edge of edges) {
      if (!graph.hasNode(edge.from) || !graph.hasNode(edge.to)) continue
      try {
        graph.addEdge(edge.from, edge.to, {
          color: EDGE_COLORS[edge.kind as EdgeKind] || '#3b4261',
          size: 1,
          type: 'arrow',
          edgeKind: edge.kind,
          filePath: edge.file_path,
        })
      } catch {
        // skip duplicate edges in non-multi mode
      }
    }

    // Set node sizes based on degree (logarithmic)
    graph.forEachNode((nodeId) => {
      const degree = graph.degree(nodeId)
      const size = Math.min(20, Math.max(3, 3 + Math.log2(degree + 1) * 3))
      graph.setNodeAttribute(nodeId, 'size', size)
    })

    return graph
  }, [nodes, edges])

  // Start layout
  const startLayout = useCallback((graph: Graph) => {
    if (layoutRef.current) {
      layoutRef.current.kill()
      layoutRef.current = null
    }

    const settings = inferSettings(graph)
    const layout = new FA2LayoutSupervisor(graph, {
      settings: {
        ...settings,
        barnesHutOptimize: graph.order > 500,
        barnesHutTheta: 0.5,
        slowDown: 5,
      },
    })

    layout.start()
    layoutRef.current = layout

    // Auto-stop after 5 seconds
    setTimeout(() => {
      if (layout.isRunning()) {
        layout.stop()
      }
    }, 5000)

    return layout
  }, [])

  // Apply visibility filters via reducers
  useEffect(() => {
    const sigma = sigmaRef.current
    if (!sigma) return

    sigma.setSetting('nodeReducer', (nodeId, data) => {
      const kind = data.nodeKind as string
      const filePath = data.filePath as string

      let hidden = false

      // Kind filter
      if (!visibleKinds.has(kind)) {
        hidden = true
      }

      // Test file filter
      if (hideTestFiles && filePath && /_test\.|\.test\.|\.spec\.|_test\.go/.test(filePath)) {
        hidden = true
      }

      // Import filter
      if (hideImports && kind === 'import') {
        hidden = true
      }

      // Hover highlight logic — dim non-neighbors
      const hovered = hoveredNodeRef.current
      if (hovered && !hidden) {
        const graph = graphRef.current
        if (graph && hovered !== nodeId) {
          const isNeighbor = graph.hasEdge(hovered, nodeId) || graph.hasEdge(nodeId, hovered)
          if (!isNeighbor) {
            // Dim the color: parse hex and set low opacity
            const hex = (data.color as string) || '#6b7280'
            const r = parseInt(hex.slice(1, 3), 16)
            const g = parseInt(hex.slice(3, 5), 16)
            const b = parseInt(hex.slice(5, 7), 16)
            return { ...data, hidden: false, color: `rgba(${r},${g},${b},0.08)`, label: null }
          }
          // Hovered node's neighbor — make slightly brighter/larger
          return { ...data, hidden: false, zIndex: 1 }
        }
        // The hovered node itself — highlight
        if (hovered === nodeId) {
          return { ...data, hidden: false, highlighted: true, zIndex: 2 }
        }
      }

      return { ...data, hidden }
    })

    sigma.setSetting('edgeReducer', (_edge, data) => {
      const hovered = hoveredNodeRef.current
      if (hovered) {
        const graph = graphRef.current
        if (graph) {
          const [source, target] = graph.extremities(_edge)
          if (source !== hovered && target !== hovered) {
            return { ...data, hidden: true }
          }
        }
      }
      return data
    })

    sigma.refresh()
  }, [visibleKinds, hideTestFiles, hideImports])

  // Main setup effect
  useEffect(() => {
    if (!containerRef.current || nodes.length === 0) return

    const graph = buildGraph()
    graphRef.current = graph

    const sigma = new Sigma(graph, containerRef.current, {
      defaultEdgeType: 'arrow',
      edgeProgramClasses: {
        arrow: EdgeArrowProgram,
      },
      labelColor: { color: '#d4d4d8' },
      labelSize: 12,
      labelRenderedSizeThreshold: 8,
      defaultNodeColor: '#6b7280',
      defaultEdgeColor: '#3b4261',
      renderLabels: true,
      renderEdgeLabels: false,
      hideEdgesOnMove: true,
      hideLabelsOnMove: false,
      enableEdgeEvents: false,
      zIndex: true,
      allowInvalidContainer: true,
      // Disable the default hover highlight (bright white halo)
      defaultDrawNodeHover: () => {},
      nodeReducer: (nodeId, data) => {
        const kind = data.nodeKind as string
        const filePath = data.filePath as string

        let hidden = false
        const state = useStore.getState()

        if (!state.visibleKinds.has(kind)) hidden = true
        if (state.hideTestFiles && filePath && /_test\.|\.test\.|\.spec\.|_test\.go/.test(filePath)) hidden = true
        if (state.hideImports && kind === 'import') hidden = true

        return { ...data, hidden }
      },
    })

    sigmaRef.current = sigma

    // Click handler
    sigma.on('clickNode', ({ node }) => {
      const gNode = nodes.find(n => n.id === node) ?? null
      selectNode(node, gNode)
    })

    sigma.on('clickStage', () => {
      selectNode(null, null)
    })

    // Hover handler
    sigma.on('enterNode', ({ node }) => {
      hoveredNodeRef.current = node
      setHoveredNode(node)
      sigma.refresh()
    })

    sigma.on('leaveNode', () => {
      hoveredNodeRef.current = null
      setHoveredNode(null)
      sigma.refresh()
    })

    // Start ForceAtlas2 layout
    const layout = startLayout(graph)

    // Expose camera fit
    if (fitCameraRef) {
      fitCameraRef.current = () => {
        const camera = sigma.getCamera()
        camera.animatedReset({ duration: 300 })
      }
    }

    // Expose re-layout
    if (relayoutRef) {
      relayoutRef.current = () => {
        if (layout.isRunning()) {
          layout.stop()
        }
        startLayout(graph)
      }
    }

    return () => {
      if (layoutRef.current) {
        layoutRef.current.kill()
        layoutRef.current = null
      }
      sigma.kill()
      sigmaRef.current = null
      graphRef.current = null
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [nodes, edges])

  return (
    <div
      ref={containerRef}
      className="h-full w-full"
      style={{ minHeight: '400px' }}
    />
  )
}
