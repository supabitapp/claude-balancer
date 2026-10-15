package app

import (
	"crypto/rand"
	"encoding/json"
	"strings"
)

type claudeUsagePayload struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
}

type claudeUsageEvent struct {
	Usage   claudeUsagePayload `json:"usage"`
	Message struct {
		Usage claudeUsagePayload `json:"usage"`
	} `json:"message"`
}

func (u claudeUsagePayload) responseUsage() responseUsage {
	input := u.InputTokens + u.CacheReadInputTokens + u.CacheCreationInputTokens
	usage := responseUsage{
		InputTokens:  input,
		OutputTokens: u.OutputTokens,
		TotalTokens:  input + u.OutputTokens,
	}
	usage.InputDetails.CachedTokens = u.CacheReadInputTokens
	usage.InputDetails.CacheWriteTokens = u.CacheCreationInputTokens
	return usage
}

func (u *claudeUsagePayload) merge(other claudeUsagePayload) {
	u.InputTokens = max(u.InputTokens, other.InputTokens)
	u.OutputTokens = max(u.OutputTokens, other.OutputTokens)
	u.CacheCreationInputTokens = max(u.CacheCreationInputTokens, other.CacheCreationInputTokens)
	u.CacheReadInputTokens = max(u.CacheReadInputTokens, other.CacheReadInputTokens)
}

func (e claudeUsageEvent) usage() claudeUsagePayload {
	usage := e.Usage
	usage.merge(e.Message.Usage)
	return usage
}

func claudeResponseUsage(body []byte) responseUsage {
	var event claudeUsageEvent
	if json.Unmarshal(body, &event) == nil {
		return event.usage().responseUsage()
	}

	var total claudeUsagePayload
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" || data == "[DONE]" {
			continue
		}
		event = claudeUsageEvent{}
		if json.Unmarshal([]byte(data), &event) != nil {
			continue
		}
		total.merge(event.usage())
	}
	return total.responseUsage()
}

func (r claudeRoute) statsThread(model string) string {
	if keys := r.writeKeys(model); len(keys) > 0 {
		return keys[0]
	}
	return "claude:request:" + rand.Text()
}

func (r claudeRoute) turnMetadata() turnMetadata {
	thread := r.session
	if thread == "" {
		thread = r.thread
	}
	return turnMetadata{ThreadID: thread, ParentThreadID: r.parentSession}
}
