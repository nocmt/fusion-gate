package types

// ============================================================
// OpenAI Responses API — 完整类型定义
// 参考: https://platform.openai.com/docs/api-reference/responses
// ============================================================

// --------- 请求 ---------

type ResponsesRequest struct {
	Model              string            `json:"model"`
	Input              any               `json:"input,omitempty"`
	Instructions       string            `json:"instructions,omitempty"`
	Temperature        *float64          `json:"temperature,omitempty"`
	MaxOutputTokens    *int              `json:"max_output_tokens,omitempty"`
	TopP               *float64          `json:"top_p,omitempty"`
	Stream             bool              `json:"stream,omitempty"`
	Tools              []ToolItem        `json:"tools,omitempty"`
	ToolChoice         any               `json:"tool_choice,omitempty"`
	ParallelToolCalls  *bool             `json:"parallel_tool_calls,omitempty"`
	Reasoning          *ReasoningConfig  `json:"reasoning,omitempty"`
	Store              *bool             `json:"store,omitempty"`
	PreviousResponseID string            `json:"previous_response_id,omitempty"`
	Conversation       any               `json:"conversation,omitempty"`
	Metadata           map[string]string `json:"metadata,omitempty"`
	User               string            `json:"user,omitempty"`
	Include            []string          `json:"include,omitempty"`

	// FusionGate 扩展
	XGroup    string `json:"x_group,omitempty"`
	XStrategy string `json:"x_strategy,omitempty"`
}

type ReasoningConfig struct {
	Effort          string `json:"effort,omitempty"`
	GenerateSummary string `json:"generate_summary,omitempty"`
}

// --------- 工具定义 ---------

type ToolItem struct {
	Type        string `json:"type"` // "function"|"file_search"|"web_search"|"code_interpreter"|"computer_use_preview"
	Name        string `json:"name,omitempty"`
	Description string `json:"description,omitempty"`
	Parameters  any    `json:"parameters,omitempty"`
	Strict      *bool  `json:"strict,omitempty"`

	// file_search
	VectorStoreIDs []string `json:"vector_store_ids,omitempty"`
	MaxNumResults  *int     `json:"max_num_results,omitempty"`
	RankingOptions any      `json:"ranking_options,omitempty"`

	// code_interpreter
	ContainerCodeInterpreter any `json:"container,omitempty"`

	// web_search
	UserLocation      any    `json:"user_location,omitempty"`
	SearchContextSize string `json:"search_context_size,omitempty"`
}

// --------- 输入项 ---------

type InputMessage struct {
	Type    string `json:"type"` // "message"
	Role    string `json:"role"`
	Content any    `json:"content,omitempty"` // string | []ContentPart
	Status  string `json:"status,omitempty"`
}

type FunctionCallOutput struct {
	Type   string `json:"type"` // "function_call_output"
	CallID string `json:"call_id"`
	Output string `json:"output"`
}

type ContentPart struct {
	Type     string `json:"type"` // "input_text"|"input_image"|"input_file"
	Text     string `json:"text,omitempty"`
	ImageURL string `json:"image_url,omitempty"`
	Detail   string `json:"detail,omitempty"`
	FileID   string `json:"file_id,omitempty"`
	FileData string `json:"file_data,omitempty"`
	Filename string `json:"filename,omitempty"`
}

// --------- 非流式响应 ---------

type ResponsesResponse struct {
	ID                 string       `json:"id"`
	Object             string       `json:"object"` // "response"
	CreatedAt          int64        `json:"created_at"`
	Status             string       `json:"status"`
	Model              string       `json:"model"`
	Output             []OutputItem `json:"output"`
	Usage              *UsageDetail `json:"usage,omitempty"`
	Error              *ErrorDetail `json:"error,omitempty"`
	IncompleteDetails  *Incomplete  `json:"incomplete_details,omitempty"`
	ConversationID     string       `json:"conversation_id,omitempty"`
	ResponseID         string       `json:"response_id,omitempty"` // 冗余字段，表示"下一个要传的 previous_response_id"
	ParallelToolCalls  bool         `json:"parallel_tool_calls"`
	Temperature        float64      `json:"temperature,omitempty"`
	TopP               float64      `json:"top_p,omitempty"`
	MaxOutputTokens    int          `json:"max_output_tokens,omitempty"`
	PreviousResponseID string       `json:"previous_response_id,omitempty"`
}

type UsageDetail struct {
	InputTokens         int                 `json:"input_tokens"`
	OutputTokens        int                 `json:"output_tokens"`
	TotalTokens         int                 `json:"total_tokens"`
	InputTokensDetails  *InputTokensDetail  `json:"input_tokens_details,omitempty"`
	OutputTokensDetails *OutputTokensDetail `json:"output_tokens_details,omitempty"`
}
type InputTokensDetail struct {
	CachedTokens int `json:"cached_tokens,omitempty"`
}
type OutputTokensDetail struct {
	ReasoningTokens int `json:"reasoning_tokens,omitempty"`
}
type ErrorDetail struct {
	Message string `json:"message"`
	Type    string `json:"type"`
	Code    string `json:"code,omitempty"`
}
type Incomplete struct {
	Reason string `json:"reason"`
}

// --------- 输出项 ---------

type OutputItem struct {
	ID     string `json:"id"`
	Type   string `json:"type"`             // "message"|"function_call"|"web_search_call"|"file_search_call"|"code_interpreter_call"
	Status string `json:"status,omitempty"` // "in_progress"|"completed"|"incomplete"

	// message
	Role    string          `json:"role,omitempty"`
	Content []OutputContent `json:"content,omitempty"`

	// function_call
	CallID    string `json:"call_id,omitempty"`
	Name      string `json:"name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

type OutputContent struct {
	Type        string       `json:"type"` // "output_text"|"refusal"|"input_image"
	Text        string       `json:"text,omitempty"`
	Refusal     string       `json:"refusal,omitempty"`
	Annotations []Annotation `json:"annotations,omitempty"`
}

type Annotation struct {
	Type       string `json:"type"`
	Text       string `json:"text,omitempty"`
	URL        string `json:"url,omitempty"`
	Title      string `json:"title,omitempty"`
	FileID     string `json:"file_id,omitempty"`
	Filename   string `json:"filename,omitempty"`
	StartIndex int    `json:"start_index,omitempty"`
	EndIndex   int    `json:"end_index,omitempty"`
}

// --------- 流式事件 data 结构 ---------

type EventResponseCreated struct {
	ID             string `json:"id"`
	Object         string `json:"object"`
	CreatedAt      int64  `json:"created_at"`
	Model          string `json:"model"`
	Status         string `json:"status"`
	ConversationID string `json:"conversation_id,omitempty"`
}

type EventOutputItemAdded struct {
	OutputIndex int        `json:"output_index"`
	Item        OutputItem `json:"item"`
}

type EventContentPartAdded struct {
	OutputIndex  int           `json:"output_index"`
	ItemID       string        `json:"item_id"`
	ContentIndex int           `json:"content_index"`
	Part         OutputContent `json:"part"`
}

type EventTextDelta struct {
	OutputIndex  int    `json:"output_index"`
	ItemID       string `json:"item_id"`
	ContentIndex int    `json:"content_index"`
	Delta        string `json:"delta"`
}

type EventTextDone struct {
	OutputIndex  int    `json:"output_index"`
	ItemID       string `json:"item_id"`
	ContentIndex int    `json:"content_index"`
	Text         string `json:"text"`
}

type EventContentPartDone struct {
	OutputIndex  int           `json:"output_index"`
	ItemID       string        `json:"item_id"`
	ContentIndex int           `json:"content_index"`
	Part         OutputContent `json:"part"`
}

type EventFunctionCallArgsDelta struct {
	OutputIndex  int    `json:"output_index"`
	ItemID       string `json:"item_id"`
	ContentIndex int    `json:"content_index,omitempty"`
	Delta        string `json:"delta"`
}

type EventFunctionCallArgsDone struct {
	OutputIndex  int    `json:"output_index"`
	ItemID       string `json:"item_id"`
	ContentIndex int    `json:"content_index,omitempty"`
	Arguments    string `json:"arguments"`
}

type EventOutputItemDone struct {
	OutputIndex int        `json:"output_index"`
	Item        OutputItem `json:"item"`
}

type EventResponseCompleted struct {
	ID                string       `json:"id"`
	Object            string       `json:"object"`
	CreatedAt         int64        `json:"created_at"`
	Model             string       `json:"model"`
	Status            string       `json:"status"`
	Output            []OutputItem `json:"output"`
	Usage             *UsageDetail `json:"usage,omitempty"`
	ConversationID    string       `json:"conversation_id,omitempty"`
	ResponseID        string       `json:"response_id,omitempty"`
	IncompleteDetails *Incomplete  `json:"incomplete_details,omitempty"`
}

type EventResponseFailed struct {
	ID     string       `json:"id"`
	Status string       `json:"status"`
	Error  *ErrorDetail `json:"error"`
}
