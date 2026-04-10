import type {
  HealthResponse, ToolInfo, GraphStats, ToolResponse, GraphData,
  SubGraph, GortexNode, GraphChangeEvent, CommunityResult, Community,
  Process, IndexHealth,
} from './types'

const BRIDGE_URL = process.env.NEXT_PUBLIC_GORTEX_URL || 'http://localhost:4747'
const WEB_URL = process.env.NEXT_PUBLIC_GORTEX_WEB_URL || BRIDGE_URL

// --- Bridge API ---

async function bridgeFetch(path: string, options?: RequestInit): Promise<Response> {
  const res = await fetch(`${BRIDGE_URL}${path}`, {
    ...options,
    headers: { 'Content-Type': 'application/json', ...options?.headers },
  })
  if (!res.ok) {
    const text = await res.text()
    throw new Error(`Bridge API error ${res.status}: ${text}`)
  }
  return res
}

async function callTool(name: string, args: Record<string, unknown> = {}): Promise<string> {
  const res = await bridgeFetch(`/tool/${name}`, {
    method: 'POST',
    body: JSON.stringify({ arguments: args }),
  })
  const data: ToolResponse = await res.json()
  if (data.isError) {
    throw new Error(data.content?.[0]?.text || 'Tool call failed')
  }
  return data.content?.map(c => c.text).join('\n') || ''
}

async function callToolJSON<T>(name: string, args: Record<string, unknown> = {}): Promise<T> {
  const text = await callTool(name, args)
  return JSON.parse(text) as T
}

// --- Public API ---

export const api = {
  // Health & stats
  health: async (): Promise<HealthResponse> => {
    const res = await bridgeFetch('/health')
    return res.json()
  },

  tools: async (): Promise<ToolInfo[]> => {
    const res = await bridgeFetch('/tools')
    return res.json()
  },

  stats: async (): Promise<GraphStats> => {
    const res = await bridgeFetch('/stats')
    return res.json()
  },

  // Graph data (from web API)
  getGraph: async (): Promise<GraphData> => {
    const res = await fetch(`${WEB_URL}/api/graph`)
    return res.json()
  },

  getFileGraph: async (path: string): Promise<SubGraph> => {
    const res = await fetch(`${WEB_URL}/api/graph/file?path=${encodeURIComponent(path)}`)
    return res.json()
  },

  getCluster: async (id: string, radius = 2): Promise<SubGraph> => {
    const res = await fetch(`${WEB_URL}/api/graph/cluster?id=${encodeURIComponent(id)}&radius=${radius}`)
    return res.json()
  },

  // MCP tool wrappers
  searchSymbols: async (query: string, limit = 20): Promise<string> => {
    return callTool('search_symbols', { query, limit, compact: true })
  },

  getSymbol: async (id: string): Promise<GortexNode | null> => {
    try {
      return await callToolJSON<GortexNode>('get_symbol', { id })
    } catch { return null }
  },

  getSymbolSource: async (id: string): Promise<string> => {
    const result = await callTool('get_symbol_source', { id })
    try {
      const parsed = JSON.parse(result)
      return parsed.source || result
    } catch { return result }
  },

  getSymbolSignature: async (id: string): Promise<string> => {
    return callTool('get_symbol_signature', { id })
  },

  getCommunities: async (): Promise<CommunityResult> => {
    return callToolJSON<CommunityResult>('get_communities', {})
  },

  getCommunity: async (id: string): Promise<Community> => {
    return callToolJSON<Community>('get_community', { id })
  },

  getProcesses: async (): Promise<{ processes: Process[] }> => {
    return callToolJSON<{ processes: Process[] }>('get_processes', {})
  },

  getProcess: async (id: string): Promise<Process> => {
    return callToolJSON<Process>('get_process', { id })
  },

  getCallers: async (id: string, depth = 2): Promise<SubGraph> => {
    return callToolJSON<SubGraph>('get_callers', { function_id: id, depth, compact: true })
  },

  getCallChain: async (id: string, depth = 2): Promise<SubGraph> => {
    return callToolJSON<SubGraph>('get_call_chain', { function_id: id, depth, compact: true })
  },

  findUsages: async (id: string): Promise<SubGraph> => {
    return callToolJSON<SubGraph>('find_usages', { id, compact: true })
  },

  getDependencies: async (id: string): Promise<SubGraph> => {
    return callToolJSON<SubGraph>('get_dependencies', { id })
  },

  getDependents: async (id: string): Promise<SubGraph> => {
    return callToolJSON<SubGraph>('get_dependents', { id })
  },

  explainChangeImpact: async (symbolIds: string): Promise<unknown> => {
    return callToolJSON('explain_change_impact', { symbol_ids: symbolIds })
  },

  findDeadCode: async (): Promise<unknown> => {
    return callToolJSON('find_dead_code', {})
  },

  findHotspots: async (): Promise<unknown> => {
    return callToolJSON('find_hotspots', {})
  },

  findCycles: async (): Promise<unknown> => {
    return callToolJSON('find_cycles', {})
  },

  indexHealth: async (): Promise<IndexHealth> => {
    return callToolJSON<IndexHealth>('index_health', {})
  },

  graphStats: async (): Promise<GraphStats> => {
    return callToolJSON<GraphStats>('graph_stats', {})
  },

  // Raw tool call
  callTool,
  callToolJSON,

  // SSE
  subscribeEvents: (callback: (event: GraphChangeEvent) => void): EventSource => {
    const es = new EventSource(`${WEB_URL}/api/events`)
    es.addEventListener('graph_change', (e) => {
      try {
        const data = JSON.parse(e.data) as GraphChangeEvent
        callback(data)
      } catch { /* ignore parse errors */ }
    })
    return es
  },
}
