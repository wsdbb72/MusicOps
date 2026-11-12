package tokenledger

import (
	"bytes"
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
	RecheckAtNS        int64  `json:"recheck_at_ns,omitempty"`
	ErrorClass         string `json:"error_class,omitempty"`
	ProductionVerified bool   `json:"production_verified"`
	// ReadBase is the generation seen before this read. It is not stored.
	// A response that started from an older generation cannot replace a newer one.
	ReadBase    uint64 `json:"-"`
	ReadBaseSet bool   `json:"-"`
}

var pageBalance = regexp.MustCompile(`(?i)(?:FL\s*令牌|令牌|Tokens?)\s*[:：]\s*([\d,]+(?:\s*\+\s*[\d,]+)*)`)
var grouped = regexp.MustCompile(`^(?:\d+|\d{1,3}(?:,\d{3})+)$`)

// ParseBalance reads the fields already used by the Python reader:
// personal.freeleechTokens, stats.tokens, userstats.tokens, then one unique page total.
// A present but unreadable field does not fall through to another number.
func ParseBalance(site string, body []byte) Observation {
	obs := Observation{Contract: BalanceContract, Site: site}
	if !tokenSite(site) || len(body) == 0 {
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

// ParsePageBalance reads the token count from a rendered user page that was
// fetched with this account's own session. A page alone proves nothing about
// which account it describes, so the count is only accepted when the same page
// links to that account id (user.php?id=<id>): the binding comes from the
// document, not from the caller's assumption.
func ParsePageBalance(site string, page []byte, accountID string) Observation {
	obs := ParseBalance(site, page)
	if obs.Known || obs.Source != "page" || accountID == "" {
		return obs
	}
	if !strings.Contains(string(page), "user.php?id="+accountID) {
		obs.ErrorClass = "binding"
		return obs
	}
	obs.Known = true
	obs.AccountID = accountID
	obs.ErrorClass = ""
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

func cookieKey(site, cookieID string) []byte {
	return []byte("fl/cookie/" + site + "/" + cookieID)
}

// CommitObservation stores a parsed balance. An older generation, or a read that
// started before the stored generation, does not replace a newer observation.
// The same cookie fingerprint cannot be recorded as two account ids.
func (l *Ledger) CommitObservation(obs Observation) (Observation, error) {
	if l == nil || l.engine == nil || !tokenSite(obs.Site) {
		return Observation{}, dataengine.ErrInvalidRequest
	}
	obs.Contract = BalanceContract
	obs.ProductionVerified = false
	account := obs.AccountID
	if account == "" {
		account = "unbound"
	}
	key := obsKey(obs.Site, account)
	for range 2 {
		var current Observation
		var raw []byte
		var found bool
		var bound string
		var cookieRaw []byte
		var cookieFound bool
		err := l.engine.View(func(v *dataengine.View) error {
			value, ok, err := v.Get(key)
			if err != nil {
				return err
			}
			if ok {
				found = true
				raw = bytes.Clone(value)
				if json.Unmarshal(value, &current) != nil {
					return dataengine.ErrInvalidRequest
				}
			}
			if obs.CookieID == "" || obs.AccountID == "" {
				return nil
			}
			value, ok, err = v.Get(cookieKey(obs.Site, obs.CookieID))
			if err != nil || !ok {
				return err
			}
			cookieFound = true
			cookieRaw = bytes.Clone(value)
			bound = string(value)
			return nil
		})
		if err != nil {
			return Observation{}, err
		}
		if cookieFound && bound != obs.AccountID {
			obs.Known = false
			obs.Balance = 0
			obs.ErrorClass = "binding"
			obs.Generation = 0
			return obs, nil
		}
		if obs.Generation == 0 {
			if obs.ReadBaseSet && found && current.Generation != obs.ReadBase {
				return current, nil
			}
			if obs.ReadBaseSet && !found && obs.ReadBase != 0 {
				return current, nil
			}
			if found {
				obs.Generation = current.Generation + 1
			} else {
				obs.Generation = 1
			}
		} else if found && obs.Generation <= current.Generation {
			return current, nil
		}
		encoded, err := json.Marshal(obs)
		if err != nil {
			return Observation{}, err
		}
		conditions := []dataengine.Condition{{Key: key, Absent: !found, Value: raw}}
		mutations := []dataengine.Mutation{{Key: key, Value: encoded}}
		if obs.Known && obs.CookieID != "" && obs.AccountID != "" {
			conditions = append(conditions, dataengine.Condition{Key: cookieKey(obs.Site, obs.CookieID), Absent: !cookieFound, Value: cookieRaw})
			mutations = append(mutations, dataengine.Mutation{Key: cookieKey(obs.Site, obs.CookieID), Value: []byte(obs.AccountID)})
		}
		ok, err := l.engine.CompareAndSwap(conditions, mutations)
		if err != nil {
			return Observation{}, err
		}
		if ok {
			return obs, nil
		}
	}
	stored, found, err := l.Load(obs.Site, account)
	if err != nil || found {
		return stored, err
	}
	return Observation{}, dataengine.ErrInvalidRequest
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
	cookieMismatch := quote.CookieID != "" && obs.CookieID != "" && quote.CookieID != obs.CookieID
	if !found || !obs.Known || obs.ErrorClass != "" || obs.AccountID != accountID || cookieMismatch {
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
	quote.Budget = capBudget(quote.Budget, obs.Balance)
	return l.Reserve(now, quote)
}

// capBudget turns an unset or oversized batch cap into a value Reserve can
// store. An explicit cap within the balance is kept. Zero means "no caller
// cap", so the batch is limited by the balance and by maxQuoteBudget. A
// balance above maxQuoteBudget must not be copied through: validateQuote
// rejects Budget > maxQuoteBudget and that error used to fail the download
// before any token request.
func capBudget(budget, balance int) int {
	if budget <= 0 || budget > balance {
		budget = balance
	}
	if budget > maxQuoteBudget {
		budget = maxQuoteBudget
	}
	if budget < 0 {
		return 0
	}
	return budget
}
