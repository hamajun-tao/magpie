package provider

import (
	"encoding/json"
	"strings"
)

// A 200 HTML error page, {}, or an empty generation does not prove that a
// relay can run the model. Accept actual text or reasoning, without judging
// whether the model followed the wording of the test prompt exactly.
func healthAnswer(b []byte) bool {
	var v struct {
		Error   json.RawMessage `json:"error"`
		Status  string          `json:"status"`
		Choices []struct {
			Text    string `json:"text"`
			Message struct {
				Content          json.RawMessage `json:"content"`
				Reasoning        string          `json:"reasoning"`
				ReasoningContent string          `json:"reasoning_content"`
			} `json:"message"`
		} `json:"choices"`
		Content json.RawMessage `json:"content"`
		Output  []struct {
			Content json.RawMessage `json:"content"`
			Summary json.RawMessage `json:"summary"`
		} `json:"output"`
	}
	if json.Unmarshal(b, &v) != nil || len(v.Error) > 0 && string(v.Error) != "null" || v.Status == "failed" {
		return false
	}
	for _, c := range v.Choices {
		if healthText(c.Message.Content) || strings.TrimSpace(c.Text+c.Message.Reasoning+c.Message.ReasoningContent) != "" {
			return true
		}
	}
	if healthText(v.Content) {
		return true
	}
	for _, o := range v.Output {
		if healthText(o.Content) || healthText(o.Summary) {
			return true
		}
	}
	return false
}

func healthText(b []byte) bool {
	var s string
	if json.Unmarshal(b, &s) == nil {
		return strings.TrimSpace(s) != ""
	}
	var blocks []struct {
		Text string `json:"text"`
	}
	if json.Unmarshal(b, &blocks) == nil {
		for _, block := range blocks {
			if strings.TrimSpace(block.Text) != "" {
				return true
			}
		}
	}
	return false
}

func healthStreamContent(b []byte) bool {
	var v struct {
		Delta        json.RawMessage `json:"delta"`
		ContentBlock struct {
			Text string `json:"text"`
		} `json:"content_block"`
	}
	if json.Unmarshal(b, &v) != nil {
		return false
	}
	if healthText(v.Delta) || strings.TrimSpace(v.ContentBlock.Text) != "" {
		return true
	}
	var d struct {
		Text     string `json:"text"`
		Thinking string `json:"thinking"`
	}
	return json.Unmarshal(v.Delta, &d) == nil && strings.TrimSpace(d.Text+d.Thinking) != ""
}
