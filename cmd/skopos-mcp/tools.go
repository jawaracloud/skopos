package main

type mcpTool struct {
	Name        string         `json:"name"`
	Description string         `json:"description"`
	InputSchema map[string]any `json:"inputSchema"`
}

func tools() []mcpTool {
	limit := map[string]any{"type": "integer", "minimum": 1, "maximum": 50}
	detail := map[string]any{
		"type": "string", "enum": []string{"minimal", "standard", "full"},
		"description": "minimal is the default and is token-efficient.",
	}
	include := map[string]any{
		"type":        "string",
		"description": "Comma-separated optional blocks: labels,query,runbook,related,caveats,provenance,fields,downstream.",
	}
	service := map[string]any{"type": "string", "description": "Service name, for example checkout."}
	query := map[string]any{"type": "string", "description": "Query expression."}
	timeString := map[string]any{"type": "string", "description": "RFC3339 or Unix timestamp."}
	return []mcpTool{
		{
			Name: "list_metrics",
			Description: "List compact metric cards. Use this before querying to avoid guessing. " +
				"Returns name/type/unit/role/service/flags only.",
			InputSchema: objectSchema(map[string]any{
				"service": service,
				"role": map[string]any{
					"type": "string",
					"enum": []string{"latency", "traffic", "error_rate", "saturation", "resource", "business_kpi", "build_info"},
				},
				"limit": limit,
			}, nil),
		},
		{
			Name: "get_metric_context",
			Description: "Get one metric's explicit semantic context. Default detail=minimal. " +
				"known:false means the catalog has no entry; do not invent one.",
			InputSchema: objectSchema(map[string]any{
				"name":    map[string]any{"type": "string", "description": "Prometheus metric name."},
				"detail":  detail,
				"include": include,
			}, []string{"name"}),
		},
		{
			Name:        "get_service_context",
			Description: "Get a compact service card with owner, runbook, and bounded metrics/log/trace hints.",
			InputSchema: objectSchema(map[string]any{
				"service": service,
				"limit":   map[string]any{"type": "integer", "minimum": 1, "maximum": 20},
			}, []string{"service"}),
		},
		{
			Name:        "list_logs",
			Description: "List compact log context cards, optionally filtered by service.",
			InputSchema: objectSchema(map[string]any{"service": service, "limit": limit}, nil),
		},
		{
			Name:        "get_log_context",
			Description: "Get one log stream/event context card. Use detail=minimal by default.",
			InputSchema: objectSchema(map[string]any{
				"id":      map[string]any{"type": "string", "description": "Log context id, for example checkout.error."},
				"detail":  detail,
				"include": include,
			}, []string{"id"}),
		},
		{
			Name:        "list_traces",
			Description: "List compact trace operation cards, optionally filtered by service.",
			InputSchema: objectSchema(map[string]any{"service": service, "limit": limit}, nil),
		},
		{
			Name:        "get_trace_context",
			Description: "Get one trace operation context card. Use detail=minimal by default.",
			InputSchema: objectSchema(map[string]any{
				"id":      map[string]any{"type": "string", "description": "Trace context id, for example checkout.get."},
				"detail":  detail,
				"include": include,
			}, []string{"id"}),
		},
		{
			Name:        "get_context_coverage",
			Description: "Get documented context counts for metrics, services, logs, and traces.",
			InputSchema: objectSchema(map[string]any{}, nil),
		},
		{
			Name:        "get_well_known",
			Description: "Discover the context API, signals, and query API paths.",
			InputSchema: objectSchema(map[string]any{}, nil),
		},
		{
			Name:        "query_promql",
			Description: "Run a PromQL instant query through Skopos.",
			InputSchema: objectSchema(map[string]any{"query": query, "time": timeString}, []string{"query"}),
		},
		{
			Name:        "query_logql",
			Description: "Run a LogQL range query through Skopos.",
			InputSchema: objectSchema(map[string]any{
				"query": query,
				"start": timeString,
				"end":   timeString,
				"direction": map[string]any{
					"type": "string", "enum": []string{"forward", "backward"},
				},
				"limit": limit,
			}, []string{"query"}),
		},
		{
			Name:        "search_traces",
			Description: "Search traces with TraceQL through Skopos.",
			InputSchema: objectSchema(map[string]any{
				"query": query,
				"start": timeString,
				"end":   timeString,
				"limit": limit,
			}, []string{"query"}),
		},
	}
}

func objectSchema(properties map[string]any, required []string) map[string]any {
	schema := map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties":           properties,
	}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema
}
