package question

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"strings"

	"github.com/0cv/herdr-mobile-relay/internal/textutil"
)

type Option struct {
	Index       int            `json:"index"`
	Label       string         `json:"label"`
	Description string         `json:"description"`
	Selected    bool           `json:"selected"`
	Summary     []SummaryEntry `json:"summary,omitempty"`
}

// SummaryEntry is one answered question in a final review option, kept
// structured so the phone can style questions and answers differently.
type SummaryEntry struct {
	Question string `json:"q"`
	Answer   string `json:"a"`
}

type Other struct {
	Selected    bool   `json:"selected"`
	Text        string `json:"text"`
	Label       string `json:"label,omitempty"`
	Placeholder string `json:"placeholder,omitempty"`
	AllowEmpty  bool   `json:"allow_empty,omitempty"`
	Hidden      bool   `json:"hidden,omitempty"`
}

type Focus struct {
	Kind  string `json:"-"`
	Index int    `json:"-"`
}

type Interaction struct {
	ID            string   `json:"id"`
	Kind          string   `json:"kind"`
	Question      string   `json:"question"`
	Options       []Option `json:"options"`
	Other         Other    `json:"other"`
	SubmitLabel   string   `json:"submit_label"`
	CanChat       bool     `json:"can_chat"`
	CanGoBack     bool     `json:"can_go_back"`
	QuestionIndex int      `json:"question_index,omitempty"`
	QuestionTotal int      `json:"question_total,omitempty"`

	Focus          Focus  `json:"-"`
	AllOptionCount int    `json:"-"`
	Agent          string `json:"-"`
	NotesActive    bool   `json:"-"`
}

func finishInteraction(interaction *Interaction) *Interaction {
	interaction.ID = interactionID(interaction)
	return interaction
}

func nextOrSubmit(current, total int) string {
	if current < total {
		return "Next"
	}
	return "Submit"
}

func reviewInteraction(
	agent string,
	options []Option,
	focus Focus,
	canGoBack bool,
	current, total int,
) *Interaction {
	return finishInteraction(&Interaction{
		Kind:           "single_select",
		Question:       "Review your answers and choose what to do",
		Options:        options,
		Other:          Other{Hidden: true},
		SubmitLabel:    "Continue",
		CanGoBack:      canGoBack,
		QuestionIndex:  current,
		QuestionTotal:  total,
		Focus:          focus,
		AllOptionCount: len(options),
		Agent:          agent,
	})
}

func splitOther(all []Option) ([]Option, Option) {
	return all[:len(all)-1], all[len(all)-1]
}

func interactionID(interaction *Interaction) string {
	value := struct {
		Kind        string   `json:"kind"`
		Question    string   `json:"question"`
		Options     []string `json:"options"`
		SubmitLabel string   `json:"submit_label"`
		Position    []int    `json:"position,omitempty"`
	}{
		Kind:        interaction.Kind,
		Question:    interaction.Question,
		SubmitLabel: interaction.SubmitLabel,
	}
	for _, option := range interaction.Options {
		value.Options = append(value.Options, option.Label)
	}
	if interaction.QuestionIndex > 0 && interaction.QuestionTotal > 0 {
		value.Position = []int{interaction.QuestionIndex, interaction.QuestionTotal}
	}
	data, _ := json.Marshal(value)
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])[:20]
}

// summaryLines compacts each review answer separately and keeps one answer
// per line so the phone renders a readable summary instead of a paragraph.
// Entries stay structured so questions and answers can be styled apart.
func summaryLines(entries []SummaryEntry, limit int) string {
	parts := make([]string, 0, len(entries))
	for index := range entries {
		entries[index].Question = textutil.Compact(entries[index].Question, limit)
		entries[index].Answer = textutil.Compact(entries[index].Answer, limit)
		line := strconv.Itoa(index+1) + ". " + entries[index].Question
		if entries[index].Answer != "" {
			line += ": " + entries[index].Answer
		}
		parts = append(parts, line)
	}
	return strings.Join(parts, "\n")
}

// splitSummaryEntry separates a terminal "label: value" review line into its
// question and answer halves; the label side never contains a colon.
func splitSummaryEntry(line string) SummaryEntry {
	parts := strings.SplitN(line, ": ", 2)
	if len(parts) == 2 {
		return SummaryEntry{Question: parts[0], Answer: parts[1]}
	}
	return SummaryEntry{Question: line}
}

// CustomAnswerPlaceholder marks a review answer whose typed free text the
// terminal does not repeat.
const CustomAnswerPlaceholder = "custom answer"

// SummaryKey normalizes a question so review summary entries can be matched
// with the question views the free text was typed into.
func SummaryKey(value string) string {
	return strings.ToLower(textutil.Compact(strings.TrimSuffix(strings.TrimSpace(value), "?"), 500))
}

// FillCustomAnswers replaces placeholder review answers with the recorded
// free-text answers and refreshes the plain-text summary fallback.
func FillCustomAnswers(interaction *Interaction, answers map[string]string) {
	if interaction == nil || len(answers) == 0 {
		return
	}
	for index := range interaction.Options {
		option := &interaction.Options[index]
		changed := false
		for i := range option.Summary {
			if option.Summary[i].Answer != CustomAnswerPlaceholder {
				continue
			}
			if text := answers[SummaryKey(option.Summary[i].Question)]; text != "" {
				option.Summary[i].Answer = text
				changed = true
			}
		}
		if changed {
			option.Description = summaryLines(option.Summary, 1000)
		}
	}
}

func title(value string) string {
	value = strings.ToLower(value)
	if value == "" {
		return ""
	}
	return strings.ToUpper(value[:1]) + value[1:]
}
