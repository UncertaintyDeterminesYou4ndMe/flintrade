package creds

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// TokenInfo is what the app shows about an access token. It never carries
// the token itself.
type TokenInfo struct {
	IssuedAt  time.Time
	ExpiresAt time.Time
	AppKey    string // "ak" claim: the app key this token was minted for
	Channel   string // "ac" claim, e.g. lb_papertrading
	Region    string // "region_home"
}

// Paper reports whether the token belongs to a paper-trading account.
func (t TokenInfo) Paper() bool { return strings.Contains(t.Channel, "paper") }

// ExpiresIn is the time left at now (negative once expired).
func (t TokenInfo) ExpiresIn(now time.Time) time.Duration { return t.ExpiresAt.Sub(now) }

// ParseToken decodes the JWT inside a Longbridge access token. The token is
// "<prefix>_<jwt>" (e.g. "hk_<jwt>"); the signature is not verified — this
// is for display and consistency checks, not authentication.
func ParseToken(tok string) (TokenInfo, error) {
	i := strings.Index(tok, "eyJ")
	if i < 0 {
		return TokenInfo{}, errors.New("token does not contain a JWT")
	}
	parts := strings.Split(tok[i:], ".")
	if len(parts) != 3 {
		return TokenInfo{}, fmt.Errorf("JWT has %d parts, want 3", len(parts))
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return TokenInfo{}, fmt.Errorf("JWT payload: %w", err)
	}
	var c struct {
		Exp    int64  `json:"exp"`
		Iat    int64  `json:"iat"`
		AK     string `json:"ak"`
		AC     string `json:"ac"`
		Region string `json:"region_home"`
	}
	if err := json.Unmarshal(payload, &c); err != nil {
		return TokenInfo{}, fmt.Errorf("JWT claims: %w", err)
	}
	if c.Exp == 0 {
		return TokenInfo{}, errors.New("JWT has no exp claim")
	}
	return TokenInfo{
		IssuedAt:  time.Unix(c.Iat, 0).UTC(),
		ExpiresAt: time.Unix(c.Exp, 0).UTC(),
		AppKey:    c.AK,
		Channel:   c.AC,
		Region:    c.Region,
	}, nil
}

// Problem is one finding from Check. Messages never include secret values.
type Problem struct {
	Key     string
	Message string
	Fatal   bool // the daemon cannot trade with this
}

// Check validates a credential set offline: required keys present, token
// parses, token minted for this app key, not expired, warn under warnWithin.
func Check(vals map[string]string, now time.Time, warnWithin time.Duration) (TokenInfo, []Problem) {
	var probs []Problem
	for _, k := range []string{AppKey, AppSecret, AccessToken} {
		if strings.TrimSpace(vals[k]) == "" {
			probs = append(probs, Problem{k, "missing", true})
		}
	}
	tok := vals[AccessToken]
	if tok == "" {
		return TokenInfo{}, probs
	}
	info, err := ParseToken(tok)
	if err != nil {
		return TokenInfo{}, append(probs, Problem{AccessToken, err.Error(), true})
	}
	if ak := vals[AppKey]; ak != "" && info.AppKey != "" && info.AppKey != ak {
		probs = append(probs, Problem{AccessToken, "token was issued for a different app key", true})
	}
	switch left := info.ExpiresIn(now); {
	case left <= 0:
		probs = append(probs, Problem{AccessToken, "expired on " + info.ExpiresAt.Format("2006-01-02"), true})
	case left <= warnWithin:
		probs = append(probs, Problem{AccessToken,
			fmt.Sprintf("expires in %d days (%s)", int(left.Hours()/24), info.ExpiresAt.Format("2006-01-02")), false})
	}
	if d, ok := vals[DryRun]; ok && d != "0" && d != "1" {
		probs = append(probs, Problem{DryRun, `must be "0" or "1"`, true})
	}
	return info, probs
}
