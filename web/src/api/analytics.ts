// Typed client for the control-plane analytics API (Week 7).
//
// All timestamps are UTC RFC3339. estimated_cost_nano_usd is a BASE-RATE
// estimate aggregated over priced requests; it is not an invoice or full bill.
// The Requests page and usage tiles show this framing next to the numbers.

export type GatewayRequest = {
  id: string;
  project_id: string;
  project_name: string;
  virtual_key_id: string;
  virtual_key_prefix: string;
  provider: string;
  model: string;
  is_stream: boolean;
  status: "in_progress" | "succeeded" | "failed";
  started_at: string;
  first_chunk_at: string | null;
  completed_at: string | null;
  latency_ms: number | null;
  ttft_ms: number | null;
  upstream_http_status: number | null;
  error_category: string | null;
  retry_count: number;
  prompt_tokens: number | null;
  completion_tokens: number | null;
  total_tokens: number | null;
  usage_source: string | null;
  pricing_id: string | null;
  estimated_cost_nano_usd: number | null;
  upstream_request_id: string | null;
  trace_id: string | null;
  created_at: string;
};

export type RequestPage = {
  items: GatewayRequest[];
  next_cursor: string | null;
  has_more: boolean;
};

export type UsageSummary = {
  from: string;
  to: string;
  requests_total: number;
  requests_succeeded: number;
  requests_failed: number;
  priced_requests: number;
  unpriced_requests: number;
  error_rate: number | null;
  prompt_tokens: number;
  completion_tokens: number;
  total_tokens: number;
  estimated_cost_nano_usd: number;
  avg_latency_ms: number | null;
  avg_ttft_ms: number | null;
  generated_at: string;
};

export type UsagePoint = {
  ts: string;
  requests_total: number;
  requests_succeeded: number;
  requests_failed: number;
  priced_requests: number;
  unpriced_requests: number;
  prompt_tokens: number;
  completion_tokens: number;
  total_tokens: number;
  estimated_cost_nano_usd: number;
};

export type UsageTimeseries = {
  from: string;
  to: string;
  bucket: "hour" | "day";
  items: UsagePoint[];
};

export type UsageGroup = {
  key: string;
  key_id?: string;
  key_prefix?: string;
  requests_total: number;
  requests_failed: number;
  priced_requests: number;
  unpriced_requests: number;
  prompt_tokens: number;
  completion_tokens: number;
  total_tokens: number;
  estimated_cost_nano_usd: number;
};

export type UsageBreakdown = {
  dimension: string;
  from: string;
  to: string;
  items: UsageGroup[];
};

type QueryParams = Record<string, string | undefined>;

function isRecord(value: unknown): value is Record<string, unknown> {
  return typeof value === "object" && value !== null && !Array.isArray(value);
}

function requiredString(value: unknown, field: string): string {
  if (typeof value !== "string" || value.trim() === "") {
    throw new Error(`invalid ${field}`);
  }
  return value;
}

function optionalNumber(value: unknown): number | null {
  return typeof value === "number" ? value : null;
}

function optionalString(value: unknown): string | null {
  return typeof value === "string" ? value : null;
}

function boolValue(value: unknown): boolean {
  return value === true || value === false ? value : false;
}

export function parseGatewayRequest(value: unknown): GatewayRequest {
  if (!isRecord(value)) {
    throw new Error("invalid request row");
  }
  return {
    id: requiredString(value.id, "id"),
    project_id: requiredString(value.project_id, "project_id"),
    project_name: requiredString(value.project_name, "project_name"),
    virtual_key_id: requiredString(value.virtual_key_id, "virtual_key_id"),
    virtual_key_prefix: requiredString(value.virtual_key_prefix, "virtual_key_prefix"),
    provider: requiredString(value.provider, "provider"),
    model: requiredString(value.model, "model"),
    is_stream: boolValue(value.is_stream),
    status: value.status as GatewayRequest["status"],
    started_at: requiredString(value.started_at, "started_at"),
    first_chunk_at: optionalString(value.first_chunk_at),
    completed_at: optionalString(value.completed_at),
    latency_ms: optionalNumber(value.latency_ms),
    ttft_ms: optionalNumber(value.ttft_ms),
    upstream_http_status: optionalNumber(value.upstream_http_status),
    error_category: optionalString(value.error_category),
    retry_count: typeof value.retry_count === "number" ? value.retry_count : 0,
    prompt_tokens: optionalNumber(value.prompt_tokens),
    completion_tokens: optionalNumber(value.completion_tokens),
    total_tokens: optionalNumber(value.total_tokens),
    usage_source: optionalString(value.usage_source),
    pricing_id: optionalString(value.pricing_id),
    estimated_cost_nano_usd: optionalNumber(value.estimated_cost_nano_usd),
    upstream_request_id: optionalString(value.upstream_request_id),
    trace_id: optionalString(value.trace_id),
    created_at: requiredString(value.created_at, "created_at"),
  };
}

export function parseRequestPage(value: unknown): RequestPage {
  if (!isRecord(value) || !Array.isArray(value.items)) {
    throw new Error("invalid request page");
  }
  const hasMore = value.has_more === true;
  const nextCursor = typeof value.next_cursor === "string" ? value.next_cursor : null;
  return {
    items: value.items.map(parseGatewayRequest),
    next_cursor: nextCursor,
    has_more: hasMore,
  };
}

export function parseUsageSummary(value: unknown): UsageSummary {
  if (!isRecord(value)) {
    throw new Error("invalid usage summary");
  }
  const numberValue = (field: string): number => {
    const raw = value[field];
    if (typeof raw !== "number") {
      throw new Error(`invalid summary ${field}`);
    }
    return raw;
  };
  return {
    from: requiredString(value.from, "from"),
    to: requiredString(value.to, "to"),
    requests_total: numberValue("requests_total"),
    requests_succeeded: numberValue("requests_succeeded"),
    requests_failed: numberValue("requests_failed"),
    priced_requests: numberValue("priced_requests"),
    unpriced_requests: numberValue("unpriced_requests"),
    error_rate: optionalNumber(value.error_rate),
    prompt_tokens: numberValue("prompt_tokens"),
    completion_tokens: numberValue("completion_tokens"),
    total_tokens: numberValue("total_tokens"),
    estimated_cost_nano_usd: numberValue("estimated_cost_nano_usd"),
    avg_latency_ms: optionalNumber(value.avg_latency_ms),
    avg_ttft_ms: optionalNumber(value.avg_ttft_ms),
    generated_at: requiredString(value.generated_at, "generated_at"),
  };
}

export function parseUsageTimeseries(value: unknown): UsageTimeseries {
  if (!isRecord(value) || !Array.isArray(value.items)) {
    throw new Error("invalid usage timeseries");
  }
  const items = value.items.map((item) => {
    if (!isRecord(item)) {
      throw new Error("invalid usage point");
    }
    const numberValue = (field: string): number => (typeof item[field] === "number" ? (item[field] as number) : 0);
    return {
      ts: requiredString(item.ts, "ts"),
      requests_total: numberValue("requests_total"),
      requests_succeeded: numberValue("requests_succeeded"),
      requests_failed: numberValue("requests_failed"),
      priced_requests: numberValue("priced_requests"),
      unpriced_requests: numberValue("unpriced_requests"),
      prompt_tokens: numberValue("prompt_tokens"),
      completion_tokens: numberValue("completion_tokens"),
      total_tokens: numberValue("total_tokens"),
      estimated_cost_nano_usd: numberValue("estimated_cost_nano_usd"),
    } as UsagePoint;
  });
  return {
    from: requiredString(value.from, "from"),
    to: requiredString(value.to, "to"),
    bucket: value.bucket === "hour" ? "hour" : "day",
    items,
  };
}

export function parseUsageBreakdown(value: unknown): UsageBreakdown {
  if (!isRecord(value) || !Array.isArray(value.items)) {
    throw new Error("invalid usage breakdown");
  }
  const items = value.items.map((item) => {
    if (!isRecord(item)) {
      throw new Error("invalid usage group");
    }
    const numberValue = (field: string): number => (typeof item[field] === "number" ? (item[field] as number) : 0);
    const key = typeof item.key === "string" ? item.key : "";
    return {
      key,
      key_id: typeof item.key_id === "string" ? item.key_id : undefined,
      key_prefix: typeof item.key_prefix === "string" ? item.key_prefix : undefined,
      requests_total: numberValue("requests_total"),
      requests_failed: numberValue("requests_failed"),
      priced_requests: numberValue("priced_requests"),
      unpriced_requests: numberValue("unpriced_requests"),
      prompt_tokens: numberValue("prompt_tokens"),
      completion_tokens: numberValue("completion_tokens"),
      total_tokens: numberValue("total_tokens"),
      estimated_cost_nano_usd: numberValue("estimated_cost_nano_usd"),
    } as UsageGroup;
  });
  return {
    dimension: requiredString(value.dimension, "dimension"),
    from: requiredString(value.from, "from"),
    to: requiredString(value.to, "to"),
    items,
  };
}

export type RequestFilters = {
  projectID?: string;
  provider?: string;
  status?: string;
  stream?: boolean;
  from?: string;
  to?: string;
  limit?: number;
  cursor?: string | null;
};

function queryString(params: QueryParams): string {
  const search = new URLSearchParams();
  for (const [key, value] of Object.entries(params)) {
    if (value !== undefined && value !== "" && value !== null) {
      search.set(key, value);
    }
  }
  const encoded = search.toString();
  return encoded === "" ? "" : `?${encoded}`;
}

async function getJSON(path: string): Promise<unknown> {
  const response = await fetch(path, {
    credentials: "include",
    headers: { Accept: "application/json" },
  });
  if (!response.ok) {
    throw new Error(`analytics request failed with ${response.status}`);
  }
  return response.json();
}

export async function listRequests(filters: RequestFilters): Promise<RequestPage> {
  const params: QueryParams = {
    project_id: filters.projectID,
    provider: filters.provider,
    status: filters.status,
    stream: filters.stream === undefined ? undefined : String(filters.stream),
    from: filters.from,
    to: filters.to,
    limit: filters.limit === undefined ? undefined : String(filters.limit),
    cursor: filters.cursor ?? undefined,
  };
  return parseRequestPage(await getJSON(`/api/v1/requests${queryString(params)}`));
}

export async function fetchRequest(id: string): Promise<GatewayRequest> {
  return parseGatewayRequest(await getJSON(`/api/v1/requests/${encodeURIComponent(id)}`));
}

export type WindowOptions = { projectID?: string; from?: string; to?: string };

export async function fetchUsageSummary(window?: WindowOptions): Promise<UsageSummary> {
  const params: QueryParams = { project_id: window?.projectID, from: window?.from, to: window?.to };
  return parseUsageSummary(await getJSON(`/api/v1/usage/summary${queryString(params)}`));
}

export async function fetchUsageTimeseries(bucket: "hour" | "day", window?: WindowOptions): Promise<UsageTimeseries> {
  const params: QueryParams = { project_id: window?.projectID, from: window?.from, to: window?.to, bucket };
  return parseUsageTimeseries(await getJSON(`/api/v1/usage/timeseries${queryString(params)}`));
}

export async function fetchUsageBreakdown(dimension: "provider" | "model" | "key", window?: WindowOptions): Promise<UsageBreakdown> {
  const params: QueryParams = { project_id: window?.projectID, from: window?.from, to: window?.to, dimension };
  return parseUsageBreakdown(await getJSON(`/api/v1/usage/breakdown${queryString(params)}`));
}
