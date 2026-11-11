// Package reasoncode names why an intent ended without a fill in a form any
// language can say: a key and its arguments, stored next to the text.
//
// The daemon still writes its text to intents.reject_reason, unchanged:
// dreaming reads it, the CLI prints it and the golden files pin it. It also
// writes the same reason as a Code to intents.reject_code, which Flintrade.app
// says in the language it shows (catalog key "reason" + Key, as
// "reasonCloseBlackout"). An intent decided before codes existed has text
// only, and the app shows the text.
//
// Arguments are strings formatted as the text formats them, so every
// language shows the same numbers.
package reasoncode

import "encoding/json"

// Code is one reason: a key of Keys and its arguments.
type Code struct {
	Key  string            `json:"key"`
	Args map[string]string `json:"args,omitempty"`
}

// Keys lists every key with the names of its arguments. Flintrade.app's tests
// check that its catalogs have a message for each, with these arguments.
var Keys = map[string][]string{
	// risk gate
	"halted":                  nil,
	"noPositionToClose":       {"symbol"},
	"shortingDisabled":        nil,
	"playbookLongOnly":        {"playbook"},
	"playbookNeedsH4":         {"playbook"},
	"revengeCooldown":         {"symbol", "minutes"},
	"missingVolumeRatio":      nil,
	"volumeRatioLow":          {"ratio", "floor"},
	"closeBlackout":           {"minutesLeft", "blackout"},
	"missingEntryOrStop":      nil,
	"stopTooClose":            {"distance", "minimum"},
	"budgetBelowOneShare":     {"budget", "perShare"},
	"maxPositions":            {"max"},
	"capFull":                 {"cap"},
	"confidenceBelowOneShare": {"confidence", "scale"},
	"openRiskCap":             {"pct"},
	// executor
	"flipNeedsClose":     {"side", "symbol"},
	"entryInFlight":      nil,
	"cancelledForClose":  nil,
	"ttlExpired":         {"age", "ttl"},
	"closeAlreadyFilled": nil,
	"brokerRejected":     {"status", "orth", "session", "raw"},
	// reconciler
	"orderNoFill": {"status"},
}

// New is the reason key with its arguments, given as name, value pairs.
func New(key string, nameValues ...string) Code {
	c := Code{Key: key}
	if len(nameValues) > 0 {
		c.Args = make(map[string]string, len(nameValues)/2)
		for i := 0; i+1 < len(nameValues); i += 2 {
			c.Args[nameValues[i]] = nameValues[i+1]
		}
	}
	return c
}

// JSON is the code as stored in intents.reject_code; nil for the zero Code.
func (c Code) JSON() *string {
	if c.Key == "" {
		return nil
	}
	b, err := json.Marshal(c)
	if err != nil {
		return nil
	}
	s := string(b)
	return &s
}

// Parse reads a stored code; ok is false for an empty or unreadable one.
func Parse(s string) (Code, bool) {
	if s == "" {
		return Code{}, false
	}
	var c Code
	if json.Unmarshal([]byte(s), &c) != nil || c.Key == "" {
		return Code{}, false
	}
	return c, true
}

// Valid: the key is known and the arguments are exactly its own.
func (c Code) Valid() bool {
	names, ok := Keys[c.Key]
	if !ok || len(names) != len(c.Args) {
		return false
	}
	for _, n := range names {
		if _, ok := c.Args[n]; !ok {
			return false
		}
	}
	return true
}
