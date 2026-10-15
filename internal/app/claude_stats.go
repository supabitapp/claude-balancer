package app

import (
	"encoding/json"
	"strings"
	"time"
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
	usage := responseUsage{
		InputTokens:  u.InputTokens,
		OutputTokens: u.OutputTokens,
		TotalTokens:  u.InputTokens + u.OutputTokens,
	}
	usage.InputDetails.CachedTokens = u.CacheReadInputTokens
	usage.InputDetails.CacheWriteTokens = u.CacheCreationInputTokens
	return usage
}

func claudeResponseUsage(body []byte) responseUsage {
	var event claudeUsageEvent
	if json.Unmarshal(body, &event) == nil {
		usage := event.Usage.responseUsage()
		usage.add(event.Message.Usage.responseUsage())
		return usage
	}

	var total responseUsage
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
		usage := event.Usage.responseUsage()
		usage.add(event.Message.Usage.responseUsage())
		total.add(usage)
	}
	return total
}

func (s *Stats) recordClaudeResponse(at time.Time, account, model string, usage responseUsage) {
	if s == nil || account == "" {
		return
	}
	s.recordAccepted(at, "", "", apiKeyIdentity{}, account, model, "", "", transportHTTP, turnMetadata{}, true)
	if !usage.empty() {
		s.recordUsage("", account, model, "", "", usage)
	}
}
