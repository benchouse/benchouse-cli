package thoughtspot

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/benchouse/benchouse-cli/internal/agent"
	"github.com/benchouse/benchouse-cli/internal/turns"
)

// The secondary route: the signed-in user's own saved Spotter
// conversations (ThoughtSpot 26.7.0.cl or later, CAN_USE_SPOTTER). Unsaved
// conversations are not persisted, and other users' conversations are not
// visible, so this route sees a fraction of what the analytics model holds.

const (
	convsPath    = "/ai/agent/conversations"
	convsPage    = 50
	maxSavedList = 10000 // conversation headers listed at most
)

func convsList(offset int) string {
	return convsPath + "?" + url.Values{"limit": {strconv.Itoa(convsPage)}, "offset": {strconv.Itoa(offset)}, "skip_empty": {"true"}}.Encode()
}

func messagesPath(id string) string { return convsPath + "/" + id + "/messages" }

func answerPath(conv, answer string) string {
	return convsPath + "/" + conv + "/answers/" + answer + "/details"
}

var (
	convsListStatement = "GET " + apiPrefix + strings.Replace(convsList(0), "offset=0", "offset=<0, 50, ...>", 1)
	messagesStatement  = "GET " + apiPrefix + messagesPath("<conversation_identifier>")
	answerStatement    = "GET " + apiPrefix + answerPath("<conversation_identifier>", "<answer_id>")
)

type convHeader struct {
	ID        string `json:"conversation_identifier"`
	Title     string `json:"conversation_title"`
	CreatedAt any    `json:"created_at"`
	UpdatedAt any    `json:"updated_at"`
	Sources   []struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	} `json:"data_source_names"`
	AnalystID *string `json:"analyst_id"`

	last time.Time
}

type convList struct {
	Conversations []convHeader `json:"conversations"`
	HasMore       bool         `json:"has_more"`
}

type messageList struct {
	Messages []struct {
		MessageID  string `json:"message_id"`
		Timestamp  any    `json:"timestamp_in_millis"`
		UserPrompt *struct {
			Message     string `json:"message"`
			Attachments []any  `json:"attachments"`
		} `json:"user_prompt"`
		ResponseItems []struct {
			Type     string `json:"type"`
			AnswerID string `json:"answer_id"`
			Content  any    `json:"content"`
		} `json:"response_items"`
	} `json:"messages"`
}

var savedColumns = turns.NewColumns(turns.ColConversationID, turns.ColPosition, turns.ColUserID,
	turns.ColSemanticModelRef, turns.ColMessages, turns.ColToolCalls, turns.ColExtra)

// readSaved lists the user's saved conversations, keeps the most recently
// updated in the window, and reads each one's messages and answers.
func readSaved(ctx context.Context, cl *client, userID string, opts agent.Options, res *agent.Result, emit agent.Emit) error {
	var all []convHeader
	listed := 0
	for offset := 0; ; offset += convsPage {
		var l convList
		if err := cl.get(ctx, convsList(offset), &l); err != nil {
			if offset == 0 {
				return err
			}
			res.Notes = append(res.Notes, "stopped listing saved conversations: "+agent.Clip(err))
			break
		}
		res.Tally(purposeConvs, convsListStatement, len(l.Conversations))
		listed += len(l.Conversations)
		for _, c := range l.Conversations {
			if c.ID == "" {
				continue
			}
			c.last, _ = parseTime(c.UpdatedAt)
			if c.last.IsZero() {
				c.last, _ = parseTime(c.CreatedAt)
			}
			if !opts.Since.IsZero() && !c.last.IsZero() && c.last.Before(opts.Since) {
				continue
			}
			all = append(all, c)
		}
		if !l.HasMore || len(l.Conversations) == 0 {
			break
		}
		if listed >= maxSavedList {
			res.Notes = append(res.Notes, fmt.Sprintf("listed the first %s saved conversations only", agent.Thousands(maxSavedList)))
			break
		}
	}
	sort.SliceStable(all, func(i, j int) bool { return all[i].last.After(all[j].last) })
	if opts.MaxConversations > 0 && len(all) > opts.MaxConversations {
		res.Notes = append(res.Notes, fmt.Sprintf("stopped at --max-conversations %s: the most recently updated saved conversations were read",
			agent.Thousands(int64(opts.MaxConversations))))
		all = all[:opts.MaxConversations]
	}
	failed := 0
	for _, c := range all {
		n, err := readConversation(ctx, cl, c, userID, opts, res, emit)
		if err != nil {
			if ctx.Err() != nil || isEmitErr(err) {
				return err
			}
			failed++
			if failed <= 3 {
				res.Notes = append(res.Notes, "could not read a saved conversation: "+agent.Clip(err))
			}
			continue
		}
		if n > 0 {
			res.Conversations++
		}
	}
	if failed > 3 {
		res.Notes = append(res.Notes, fmt.Sprintf("%d saved conversations could not be read in all", failed))
	}
	res.Notes = append(res.Notes, "saved conversations are the signed-in user's own only; unsaved Spotter conversations are not persisted by ThoughtSpot")
	return nil
}

// emitErr marks an error from emit, which stops the collection.
type emitErr struct{ error }

func isEmitErr(err error) bool { _, ok := err.(emitErr); return ok }

func readConversation(ctx context.Context, cl *client, c convHeader, userID string, opts agent.Options, res *agent.Result, emit agent.Emit) (int, error) {
	id, err := pathID("conversation", c.ID)
	if err != nil {
		return 0, err
	}
	var ml messageList
	if err := cl.get(ctx, messagesPath(id), &ml); err != nil {
		return 0, err
	}
	res.Tally(purposeMessages, messagesStatement, len(ml.Messages))
	var sources []string
	for _, s := range c.Sources {
		if s.Name != "" {
			sources = append(sources, s.Name)
		}
	}
	emitted := 0
	for i, msg := range ml.Messages {
		if msg.UserPrompt == nil || strings.TrimSpace(msg.UserPrompt.Message) == "" {
			continue
		}
		at, ok := parseTime(msg.Timestamp)
		if !ok {
			at = c.last
		}
		if at.IsZero() || (!opts.Since.IsZero() && at.Before(opts.Since)) {
			continue
		}
		pos := int64(i + 1)
		t := turns.Turn{TurnID: c.ID + ":" + msg.MessageID, AskedAt: at, Question: msg.UserPrompt.Message,
			ConversationID: c.ID, Position: &pos, UserID: userID, SemanticModelRef: strings.Join(sources, ", "),
			Extra: map[string]any{"source": "spotter_saved_conversation"}}
		if msg.MessageID == "" {
			t.TurnID = c.ID + ":" + strconv.Itoa(i)
		}
		if c.Title != "" {
			t.Extra["conversation_title"] = c.Title
		}
		if c.AnalystID != nil && *c.AnalystID != "" {
			t.Extra["analyst_id"] = *c.AnalystID
		}
		var texts []string
		for _, it := range msg.ResponseItems {
			content := str(it.Content)
			switch it.Type {
			case "text":
				if strings.TrimSpace(content) != "" {
					texts = append(texts, content)
				}
			case "answer":
				tc := turns.ToolCall{Name: "thoughtspot_answer", Input: map[string]any{"answer_id": it.AnswerID}}
				if content != "" {
					tc.Output = map[string]any{"content": content}
				}
				if it.AnswerID != "" {
					if d, err := answerDetails(ctx, cl, id, it.AnswerID, res); err == nil {
						tc.Input = d.input(it.AnswerID)
						tc.Output = d.output(content)
						tc.Status = "succeeded"
					} else {
						t.Extra["answer_details_error"] = agent.Clip(err)
					}
				}
				t.ToolCalls = append(t.ToolCalls, tc)
			case "tool_call":
				t.ToolCalls = append(t.ToolCalls, turns.ToolCall{Name: "tool_call", Output: it.Content})
			default:
				if strings.TrimSpace(content) != "" {
					t.Messages = append(t.Messages, turns.Message{Text: content, Kind: it.Type})
				}
			}
		}
		if len(texts) > 0 {
			a := strings.Join(texts, "\n\n")
			t.Answer = &a
		}
		if len(msg.UserPrompt.Attachments) > 0 {
			t.Extra["attachments"] = len(msg.UserPrompt.Attachments)
		}
		rec, err := turns.Record(t, savedColumns, fetchSaved)
		if err != nil {
			continue
		}
		if err := emit(rec); err != nil {
			return emitted, emitErr{err}
		}
		emitted++
	}
	return emitted, nil
}

type answerDetail struct {
	Title             string `json:"title"`
	Description       string `json:"description"`
	Tokens            any    `json:"tokens"`
	Formulas          any    `json:"formulas"`
	Parameters        any    `json:"parameters"`
	VisualizationType string `json:"visualization_type"`
}

func (d answerDetail) input(id string) map[string]any {
	in := map[string]any{"answer_id": id}
	if d.Tokens != nil {
		in["tokens"] = d.Tokens
	}
	if d.Formulas != nil {
		in["formulas"] = d.Formulas
	}
	if d.Parameters != nil {
		in["parameters"] = d.Parameters
	}
	return in
}

func (d answerDetail) output(content string) map[string]any {
	out := map[string]any{}
	for k, v := range map[string]string{"title": d.Title, "description": d.Description, "visualization_type": d.VisualizationType, "content": content} {
		if v != "" {
			out[k] = v
		}
	}
	return out
}

func answerDetails(ctx context.Context, cl *client, conv, answer string, res *agent.Result) (answerDetail, error) {
	var d answerDetail
	aid, err := pathID("answer", answer)
	if err != nil {
		return d, err
	}
	err = cl.get(ctx, answerPath(conv, aid), &d)
	res.Tally(purposeAnswer, answerStatement, map[bool]int{true: 1}[err == nil])
	return d, err
}

// explainSaved names what to fix for the saved conversations route.
func explainSaved(err error) error {
	switch statusOf(err) {
	case http.StatusForbidden:
		return fmt.Errorf("ThoughtSpot refused the saved conversations (403): the user needs the CAN_USE_SPOTTER privilege")
	case http.StatusNotFound:
		return fmt.Errorf("ThoughtSpot has no %s%s (404): the conversation APIs need ThoughtSpot 26.7.0.cl or later", apiPrefix, convsPath)
	}
	return err
}
