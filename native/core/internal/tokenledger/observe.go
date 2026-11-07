package tokenledger

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"html"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/wsdbb72/MusicOps/native/core/internal/dataengine"
)

const (
	BalanceContract = "gazelle-balance.v1"
	maxBalance      = 1_000_000
)

// Observation is a parsed account balance. ProductionVerified stays false until a live site is checked.
type Observation struct {
	Contract           string `json:"contract"`
	Site               string `json:"site"`
	AccountID          string `json:"account_id,omitempty"`
	CookieID           string `json:"cookie_id,omitempty"`
	Balance            int    `json:"balance,omitempty"`
	Known              bool   `json:"known"`
	Source             string `json:"source,omitempty"`
	ObservedAtNS       int64  `json:"observed_at_ns,omitempty"`
	Generation         uint64 `json:"generation"`
	ErrorClass         string `json:"error_class,omitempty"`
	ProductionVerified bool   `json:"production_verified"`
}

var pageBalance = regexp.MustCompile(`(?i)(?:FL\s*令牌|令牌|Tokens?)\s*[:：]\s*([\d,]+(?:\s*\+\s*[\d,]+)*)`)
var grouped = regexp.MustCompile(`^(?:\d+|\d{1,3}(?:,\d{3})+)$`)

// ParseBalance reads the fields already used by the Python reader:
// personal.freeleechTokens, stats.tokens, userstats.tokens, then one unique page total.
// A present but unreadable field does not fall through to another number.
func ParseBalance(site string, body []byte) Observation {
	obs := Observation{Contract: BalanceContract, Site: site}
	if site != "dicmusic" && site != "orpheus" || len(body) == 0 {
		obs.ErrorClass = ClassBalanceUnknown
		return obs
	}
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" {
		obs.ErrorClass = ClassBalanceUnknown
		return obs
	}
	if strings.HasPrefix(trimmed, "<") || strings.HasPrefix(strings.ToLower(trimmed), "<!doctype") {
		return parsePage(obs, trimmed)
	}
	var data map[string]any
	if json.Unmarshal(body, &data) != nil || data["status"] != "success" {
		obs.ErrorClass = "parse"
		return obs
	}
	response, _ := data["response"].(map[string]any)
	if response == nil {
		obs.ErrorClass = "parse"
		return obs
	}
	obs.AccountID = stringID(response["id"])
	if obs.AccountID == "" {
		obs.ErrorClass = "binding"
		return obs
	}
	for _, item := range []struct{ block, key, source string }{
		{"personal", "freeleechTokens", "personal.freeleechTokens"},
		{"stats", "tokens", "stats.tokens"},
		{"userstats", "tokens", "userstats.tokens"},
	} {
		block, exists := response[item.block]
		if !exists || block == nil {
			continue
		}
		object, ok := block.(map[string]any)
		if !ok {
			obs.ErrorClass = "parse"
			return obs
		}
		raw, present := object[item.key]
		if !present {
			continue
		}
		value, ok := countToken(raw)
		if !ok {
			obs.ErrorClass = "parse"
			return obs
		}
		obs.Known = true
		obs.Balance = value
		obs.Source = item.source
		return obs
	}
	obs.ErrorClass = ClassBalanceUnknown
	return obs
}

func parsePage(obs Observation, page string) Observation {
	lower := strings.ToLower(page)
	if strings.Contains(lower, "login.php") || strings.Contains(lower, `name="password"`) || strings.Contains(lower, "name='password'") {
		obs.ErrorClass = "auth"
		return obs
	}
	visible := strings.Join(strings.Fields(html.UnescapeString(stripTags(page))), " ")
	found := map[int]bool{}
	for _, match := range pageBalance.FindAllStringSubmatch(visible, -1) {
		parts := regexp.MustCompile(`\s*\+\s*`).Split(match[1], -1)
		sum := 0
		for _, part := range parts {
			value, ok := countToken(strings.TrimSpace(part))
			if !ok {
				obs.ErrorClass = "parse"
				return obs
			}
			sum += value
		}
		found[sum] = true
	}
	if len(found) != 1 {
		obs.ErrorClass = ClassBalanceUnknown
		return obs
	}
	for value := range found {
		obs.Balance = value
	}
	obs.Source = "page"
	obs.Known = false
	obs.ErrorClass = "binding"
	return obs
}

func countToken(value any) (int, bool) {
	switch typed := value.(type) {
	case int:
		if typed < 0 || typed > maxBalance {
			return 0, false
		}
		return typed, true
	case float64:
		if typed != float64(int(typed)) || typed < 0 || typed > maxBalance {
			return 0, false
		}
		return int(typed), true
	case string:
		text := strings.TrimSpace(typed)
		if !grouped.MatchString(text) {
			return 0, false
		}
		parsed, err := strconv.Atoi(strings.ReplaceAll(text, ",", ""))
		if err != nil || parsed < 0 || parsed > maxBalance {
			return 0, false
		}
		return parsed, true
	case json.Number:
		return countToken(typed.String())
	default:
		return 0, false
	}
}

func stringID(value any) string {
	switch typed := value.(type) {
	case string:
		if typed == "" || typed == "0" {
			return ""
		}
		return typed
	case float64:
		if typed != float64(int(typed)) || typed < 1 {
			return ""
		}
		return strconv.Itoa(int(typed))
	default:
		return ""
	}
}

func stripTags(page string) string {
	var b strings.Builder
	inside := false
	for _, r := range page {
		switch {
		case r == '<':
			inside = true
		case r == '>':
			inside = false
			b.WriteByte(' ')
		case !inside:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// Fingerprint identifies a cookie without storing it.
func Fingerprint(cookie string) string { return cookieID(cookie) }

func cookieID(cookie string) string {
	sum := sha256.Sum256([]byte(cookie))
	return hex.EncodeToString(sum[:8])
}

func obsKey(site, account string) []byte {
	return []byte("fl/obs/" + site + "/" + account)
}

// CommitObservation stores a parsed balance. An older generation does not replace a newer one.
func (l *Ledger) CommitObservation(obs Observation) (Observation, error) {
	if l == nil || l.engine == nil || (obs.Site != "dicmusic" && obs.Site != "orpheus") {
		return Observation{}, dataengine.ErrInvalidRequest
	}
	obs.Contract = BalanceContract
	obs.ProductionVerified = false
	account := obs.AccountID
	if account == "" {
		account = "unbound"
	}
	key := obsKey(obs.Site, account)
	var current Observation
	var raw []byte
	var found bool
	err := l.engine.View(func(v *dataengine.View) error {
		value, ok, err := v.Get(key)
		if err != nil || !ok {
			return err
		}
		found = true
		raw = append([]byte(nil), value...)
		if json.Unmarshal(value, &current) != nil {
			return dataengine.ErrInvalidRequest
		}
		return nil
	})
	if err != nil {
		return Observation{}, err
	}
	if found && obs.Generation > 0 && obs.Generation < current.Generation {
		return current, nil
	}
	if found && current.CookieID != "" && obs.CookieID != "" && current.CookieID == obs.CookieID && current.AccountID != "" && obs.AccountID != "" && current.AccountID != obs.AccountID {
		obs.Known = false
		obs.Balance = 0
		obs.ErrorClass = "binding"
	}
	if obs.Generation == 0 {
		obs.Generation = current.Generation + 1
	}
	encoded, err := json.Marshal(obs)
	if err != nil {
		return Observation{}, err
	}
	condition := dataengine.Condition{Key: key, Absent: !found, Value: raw}
	ok, err := l.engine.CompareAndSwap([]dataengine.Condition{condition}, []dataengine.Mutation{{Key: key, Value: encoded}})
	if err != nil {
		return Observation{}, err
	}
	if !ok {
		return l.CommitObservation(obs)
	}
	return obs, nil
}

// Load returns the stored observation. A caller-supplied number is not accepted here.
func (l *Ledger) Load(site, accountID string) (Observation, bool, error) {
	var obs Observation
	var found bool
	err := l.engine.View(func(v *dataengine.View) error {
		raw, ok, err := v.Get(obsKey(site, accountID))
		if err != nil || !ok {
			return err
		}
		found = true
		if json.Unmarshal(raw, &obs) != nil {
			return dataengine.ErrInvalidRequest
		}
		return nil
	})
	return obs, found, err
}

// ReserveObserved spends only from a stored site observation with the same account and time.
func (l *Ledger) ReserveObserved(now time.Time, quote Quote, accountID string) (Result, error) {
	obs, found, err := l.Load(quote.Site, accountID)
	if err != nil {
		return Result{}, err
	}
	if !found || !obs.Known || obs.ErrorClass != "" || obs.AccountID != accountID {
		quote.BalanceKnown = false
		quote.Balance = 0
		return l.Reserve(now, quote)
	}
	observed := time.Unix(0, obs.ObservedAtNS)
	if obs.ObservedAtNS <= 0 || now.Before(observed) || now.Sub(observed) > balanceTTL {
		quote.BalanceKnown = true
		quote.Balance = obs.Balance
		quote.ObservedAt = observed
		if obs.ObservedAtNS <= 0 {
			quote.BalanceKnown = false
		}
		return l.Reserve(now, quote)
	}
	quote.BalanceKnown = true
	quote.Balance = obs.Balance
	quote.ObservedAt = observed
	if quote.Budget <= 0 || quote.Budget > obs.Balance {
		quote.Budget = obs.Balance
	}
	return l.Reserve(now, quote)
}
