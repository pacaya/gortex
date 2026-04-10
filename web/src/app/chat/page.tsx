'use client'

import { useState } from 'react'
import { MessageSquare, Send, Settings } from 'lucide-react'
import {
  Card,
  CardHeader,
  CardTitle,
  CardDescription,
  CardContent,
} from '@/components/ui/card'
import { Input } from '@/components/ui/input'
import { Button } from '@/components/ui/button'

interface Message {
  id: string
  role: 'user' | 'assistant' | 'system'
  content: string
  timestamp: Date
}

export default function ChatPage() {
  const [messages] = useState<Message[]>([
    {
      id: 'system-1',
      role: 'system',
      content:
        'AI Chat coming soon — configure LLM provider in settings to enable this feature.',
      timestamp: new Date(),
    },
  ])
  const [input, setInput] = useState('')

  return (
    <div className="flex h-full flex-col">
      <div className="mb-4">
        <h1 className="text-xl font-semibold text-zinc-100">AI Chat</h1>
        <p className="text-sm text-zinc-500">
          Chat with AI about your codebase using Gortex context
        </p>
      </div>

      <Card className="flex flex-1 flex-col border-zinc-800 bg-zinc-900">
        <CardContent className="flex flex-1 flex-col">
          {/* Message area */}
          <div className="flex flex-1 flex-col items-center justify-center gap-4 py-12">
            <div className="flex h-12 w-12 items-center justify-center rounded-full bg-zinc-800">
              <MessageSquare className="h-6 w-6 text-zinc-500" />
            </div>

            {messages.map((msg) => (
              <div key={msg.id} className="max-w-md text-center">
                {msg.role === 'system' && (
                  <Card className="border-zinc-700 bg-zinc-800/50">
                    <CardHeader>
                      <CardTitle className="flex items-center justify-center gap-2 text-sm text-zinc-300">
                        <Settings className="h-4 w-4" />
                        Setup Required
                      </CardTitle>
                      <CardDescription className="text-zinc-400">
                        {msg.content}
                      </CardDescription>
                    </CardHeader>
                  </Card>
                )}
              </div>
            ))}

            <p className="mt-2 text-xs text-zinc-600">
              Future integration with Vercel AI SDK planned
            </p>
          </div>

          {/* Input area */}
          <div className="flex items-center gap-2 border-t border-zinc-800 pt-4">
            <Input
              value={input}
              onChange={(e) => setInput(e.target.value)}
              placeholder="Configure API key to enable"
              disabled
              className="flex-1 border-zinc-700 bg-zinc-800/50 text-zinc-400 placeholder:text-zinc-600 disabled:opacity-60"
            />
            <Button
              variant="secondary"
              size="icon"
              disabled
              className="shrink-0 bg-zinc-800 text-zinc-500"
            >
              <Send className="h-4 w-4" />
            </Button>
          </div>
        </CardContent>
      </Card>
    </div>
  )
}
