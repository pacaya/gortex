# Cross-repo API contracts

Gortex detects API contracts across repos and matches providers to consumers:

```bash
# After indexing, contracts are auto-detected
gortex track .

# Via MCP tools
contracts                        # list all detected contracts (default action)
contracts {action: "check"}      # find mismatches and orphans
```

| Contract type | Detection | Provider | Consumer |
|--------------|-----------|----------|----------|
| **HTTP routes** | Framework annotations (gin, Express, FastAPI, Spring, etc.) | Route handler | HTTP client calls (fetch, http.Get) |
| **gRPC** | Proto service definitions | Service RPC | Client stub calls |
| **GraphQL** | Schema type/field definitions | Schema | Query/mutation strings |
| **Message topics** | Pub/sub patterns across Kafka, RabbitMQ, NATS, and Redis (`KindTopic` nodes, `produces_topic` / `consumes_topic` edges); dynamic topic names suppressed | Publish calls | Subscribe calls |
| **WebSocket** | Event emit/listen patterns | `emit()` | `on()` |
| **Env vars** | `os.Getenv`, `process.env`, `.env` files | `Setenv` / `.env` | `Getenv` / `process.env` |
| **OpenAPI** | Swagger/OpenAPI spec files | Spec paths | (linked to HTTP routes) |
| **Temporal workflows** | Go SDK `worker.RegisterActivity` / Java `@ActivityInterface` / `@WorkflowInterface` annotations | Activity / workflow function (carries `temporal_role` Meta) | `workflow.ExecuteActivity` / `ExecuteChildWorkflow` / `newActivityStub` calls |

Contracts are normalized to canonical IDs (e.g., `http::GET::/api/users/{id}`) and matched across repos to detect orphan providers/consumers and mismatches.
