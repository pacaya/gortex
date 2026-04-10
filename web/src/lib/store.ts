import { create } from 'zustand'
import type { GraphStats, HealthResponse, GortexNode, GraphChangeEvent } from './types'

interface AppState {
  // Connection
  connected: boolean
  health: HealthResponse | null
  stats: GraphStats | null

  // Selection
  selectedNodeId: string | null
  selectedNode: GortexNode | null
  hoveredNodeId: string | null

  // Filters
  visibleKinds: Set<string>
  hideTestFiles: boolean
  hideImports: boolean
  searchQuery: string

  // Recent changes
  recentChanges: GraphChangeEvent[]

  // Actions
  setConnected: (v: boolean) => void
  setHealth: (h: HealthResponse) => void
  setStats: (s: GraphStats) => void
  selectNode: (id: string | null, node?: GortexNode | null) => void
  setHoveredNode: (id: string | null) => void
  toggleKind: (kind: string) => void
  setHideTestFiles: (v: boolean) => void
  setHideImports: (v: boolean) => void
  setSearchQuery: (q: string) => void
  addRecentChange: (e: GraphChangeEvent) => void
}

const ALL_KINDS = new Set(['file', 'package', 'function', 'method', 'type', 'interface', 'variable', 'import'])

export const useStore = create<AppState>((set) => ({
  connected: false,
  health: null,
  stats: null,
  selectedNodeId: null,
  selectedNode: null,
  hoveredNodeId: null,
  visibleKinds: new Set(ALL_KINDS),
  hideTestFiles: false,
  hideImports: false,
  searchQuery: '',
  recentChanges: [],

  setConnected: (connected) => set({ connected }),
  setHealth: (health) => set({ health, connected: true }),
  setStats: (stats) => set({ stats }),
  selectNode: (id, node) => set({ selectedNodeId: id, selectedNode: node ?? null }),
  setHoveredNode: (id) => set({ hoveredNodeId: id }),
  toggleKind: (kind) => set((state) => {
    const next = new Set(state.visibleKinds)
    if (next.has(kind)) next.delete(kind)
    else next.add(kind)
    return { visibleKinds: next }
  }),
  setHideTestFiles: (hideTestFiles) => set({ hideTestFiles }),
  setHideImports: (hideImports) => set({ hideImports }),
  setSearchQuery: (searchQuery) => set({ searchQuery }),
  addRecentChange: (e) => set((state) => ({
    recentChanges: [e, ...state.recentChanges].slice(0, 50),
  })),
}))
