// Package sitefree reads free and token evidence from tracker responses.
// A caller flag is not evidence. Nothing here contacts a site.
package sitefree

import (
	"encoding/json"
	"html"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode"
)

var (
	positiveID = regexp.MustCompile(`^[1-9][0-9]*$`)
	flCost     = regexp.MustCompile(`(?i)\bFL\s*(?:\((\d+)\)|\[(\d+)\])?`)
	tokenWords = regexp.MustCompile(`(?i)(?:use\s+|使用\s*)?(\d+)\s*(?:枚|个)?\s*(?:FL\s*)?(?:tokens?\b|令牌|代币)`)
	balanceRE  = regexp.MustCompile(`令牌\s*[:：]\s*(\d+)`)
)

// Decision is one offline reading of a detail response. Kind is empty when the
// page does not prove free or a single token button for this resource.
type Decision struct {
	Site       string    `json:"site"`
	ResourceID string    `json:"resource_id"`
	Personal   bool      `json:"personal"`
	Global     bool      `json:"global"`
	TokenCost  int       `json:"token_cost,omitempty"`
	Balance    *int      `json:"balance,omitempty"`
	ObservedAt time.Time `json:"observed_at"`
	Source     string    `json:"source"`
	Mode       string    `json:"mode"`
}

// Read merges Gazelle JSON and the torrent row HTML. observedAt is the time
// this process received the bytes.
func Read(site, resource string, bodies [][]byte, observedAt time.Time) (Decision, error) {
	decision := Decision{Site: site, ResourceID: resource, ObservedAt: observedAt}
	if !known(site) || !positiveID.MatchString(resource) || observedAt.IsZero() {
		return Decision{}, errInput
	}
	for _, body := range bodies {
		if len(body) == 0 {
			continue
		}
		if looksJSON(body) {
			readGazelle(&decision, body)
			continue
		}
		readHTML(&decision, string(body))
	}
	decision.Mode = mode(decision)
	if decision.Personal || decision.Global {
		decision.Source = "website"
		if decision.Personal {
			decision.Source = "api"
		}
	}
	return decision, nil
}

func mode(decision Decision) string {
	switch decision.Site {
	case "dicmusic", "orpheus":
		if decision.Personal {
			return "direct"
		}
		if decision.TokenCost > 0 && decision.Balance != nil && *decision.Balance >= decision.TokenCost {
			return "token"
		}
		return ""
	case "open.cd", "redacted":
		if decision.Personal || decision.Global {
			return "direct"
		}
		return ""
	default:
		return ""
	}
}

func readGazelle(decision *Decision, body []byte) {
	var data map[string]any
	if json.Unmarshal(body, &data) != nil || data["status"] != "success" {
		return
	}
	response, _ := data["response"].(map[string]any)
	torrent, _ := response["torrent"].(map[string]any)
	if torrent == nil || !sameID(torrent["id"], decision.ResourceID) {
		return
	}
	if torrent["isPersonalFreeleech"] == true || personalValue(torrent["freeTorrent"]) {
		decision.Personal = true
	}
	if torrent["isFreeleech"] == true || globalValue(torrent["freeTorrent"]) {
		decision.Global = true
	}
	if personal, _ := response["personal"].(map[string]any); personal != nil {
		if raw, ok := personal["freeleechTokens"]; ok {
			if value, ok := integer(raw); ok {
				decision.Balance = &value
			}
		}
	}
}

func readHTML(decision *Decision, page string) {
	if status := rowStatus(page, decision.ResourceID); status == "personal" {
		decision.Personal = true
	} else if status == "global" {
		decision.Global = true
	}
	if cost, ok := tokenButton(page, decision.ResourceID); ok {
		decision.TokenCost = cost
	}
	if match := balanceRE.FindStringSubmatch(page); match != nil {
		value, err := strconv.Atoi(match[1])
		if err == nil {
			decision.Balance = &value
		}
	}
}

func rowStatus(page, resource string) string {
	lower := strings.ToLower(page)
	needle := `id="torrent` + resource + `"`
	count := 0
	status := ""
	for _, quote := range []string{`"`, `'`} {
		needle = "id=" + quote + "torrent" + resource + quote
		from := 0
		for {
			at := strings.Index(lower[from:], strings.ToLower(needle))
			if at < 0 {
				break
			}
			at += from
			end := strings.Index(lower[at:], "</tr>")
			if end < 0 {
				return ""
			}
			row := page[at : at+end]
			count++
			if found := labelStatus(row); found != "" {
				status = found
			}
			from = at + end + 5
			if from > len(lower) {
				break
			}
		}
	}
	if count != 1 {
		return ""
	}
	return status
}

func labelStatus(row string) string {
	lower := strings.ToLower(row)
	from := 0
	for {
		at := strings.Index(lower[from:], "<strong")
		if at < 0 {
			return ""
		}
		at += from
		tagEnd := strings.Index(lower[at:], ">")
		closeAt := strings.Index(lower[at:], "</strong>")
		if tagEnd < 0 || closeAt < 0 {
			return ""
		}
		tag := row[at : at+tagEnd]
		if classContains(tag, "tl_free") {
			text := row[at+tagEnd+1 : at+closeAt]
			title := attr(tag, "title")
			return classifyLabel(text + " " + title)
		}
		from = at + closeAt + 9
	}
}

func tokenButton(page, resource string) (int, bool) {
	costs := map[int]bool{}
	lower := strings.ToLower(page)
	from := 0
	for {
		at := strings.Index(lower[from:], "<a ")
		if at < 0 {
			break
		}
		at += from
		tagEnd := strings.Index(lower[at:], ">")
		closeAt := strings.Index(lower[at:], "</a>")
		if tagEnd < 0 || closeAt < 0 {
			break
		}
		tag := page[at : at+tagEnd]
		text := html.UnescapeString(page[at+tagEnd+1:at+closeAt] + " " + attr(tag, "title"))
		href := html.UnescapeString(attr(tag, "href"))
		if cost, ok := buttonCost(href, text, resource); ok {
			costs[cost] = true
		}
		from = at + closeAt + 4
	}
	if len(costs) != 1 {
		return 0, false
	}
	for cost := range costs {
		return cost, true
	}
	return 0, false
}

func buttonCost(href, text, resource string) (int, bool) {
	parsed, err := url.Parse(href)
	if err != nil {
		return 0, false
	}
	if parsed.Path != "torrents.php" && parsed.Path != "/torrents.php" {
		return 0, false
	}
	if parsed.Query().Get("action") != "download" || parsed.Query().Get("id") != resource {
		return 0, false
	}
	token := parsed.Query().Get("usetoken")
	if parsed.Query().Get("useToken") != "" || token != "1" {
		return 0, false
	}
	match := flCost.FindStringSubmatch(text)
	if match == nil {
		return 0, false
	}
	amounts := map[int]bool{}
	for _, group := range match[1:] {
		if group == "" {
			continue
		}
		parsedAmount, err := strconv.Atoi(group)
		if err != nil || parsedAmount < 1 || parsedAmount > 100 {
			return 0, false
		}
		amounts[parsedAmount] = true
	}
	for _, group := range tokenWords.FindAllStringSubmatch(text, -1) {
		parsedAmount, err := strconv.Atoi(group[1])
		if err != nil || parsedAmount < 1 || parsedAmount > 100 {
			return 0, false
		}
		amounts[parsedAmount] = true
	}
	if len(amounts) > 1 {
		return 0, false
	}
	if len(amounts) == 0 {
		return 1, true
	}
	for amount := range amounts {
		return amount, true
	}
	return 0, false
}

func classifyLabel(value string) string {
	folded := foldLabel(value)
	switch {
	case strings.Contains(folded, "personalfreeleech") || strings.Contains(folded, "personalfree") || strings.Contains(folded, "个人免费"):
		return "personal"
	case folded == "免费" || strings.Contains(folded, "freeleech") || folded == "free":
		return "global"
	default:
		return ""
	}
}

func foldLabel(value string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(value) {
		if unicode.IsSpace(r) || r == '!' || r == '！' {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func classContains(tag, class string) bool {
	raw := strings.ToLower(attr(tag, "class"))
	for _, part := range strings.Fields(raw) {
		if part == class {
			return true
		}
	}
	return false
}

func attr(tag, name string) string {
	lower := strings.ToLower(tag)
	for _, quote := range []string{`"`, `'`} {
		needle := name + "=" + quote
		at := strings.Index(lower, needle)
		if at < 0 {
			continue
		}
		rest := tag[at+len(needle):]
		end := strings.Index(rest, quote)
		if end >= 0 {
			return rest[:end]
		}
	}
	return ""
}

func personalValue(value any) bool {
	text := strings.ToLower(strings.TrimSpace(stringify(value)))
	return text == "personal" || text == "personal freeleech"
}

func globalValue(value any) bool {
	switch value {
	case true:
		return true
	}
	text := strings.ToLower(strings.TrimSpace(stringify(value)))
	return text == "1" || text == "free" || text == "freeleech"
}

func stringify(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case float64:
		return strconv.Itoa(int(typed))
	default:
		return ""
	}
}

func integer(value any) (int, bool) {
	switch typed := value.(type) {
	case float64:
		return int(typed), true
	case string:
		parsed, err := strconv.Atoi(typed)
		return parsed, err == nil
	default:
		return 0, false
	}
}

func looksJSON(body []byte) bool {
	trimmed := strings.TrimSpace(string(body))
	return strings.HasPrefix(trimmed, "{")
}

// GroupID returns the Gazelle group id when the torrent document names this resource.
func GroupID(body []byte, resource string) string {
	var data map[string]any
	if json.Unmarshal(body, &data) != nil || data["status"] != "success" {
		return ""
	}
	response, _ := data["response"].(map[string]any)
	torrent, _ := response["torrent"].(map[string]any)
	group, _ := response["group"].(map[string]any)
	if torrent == nil || group == nil || !sameID(torrent["id"], resource) {
		return ""
	}
	if !sameID(group["id"], stringify(group["id"])) {
		return ""
	}
	return stringify(group["id"])
}

func sameID(value any, resource string) bool {
	switch typed := value.(type) {
	case string:
		return typed == resource && positiveID.MatchString(typed)
	case float64:
		if typed != float64(int(typed)) || typed < 1 {
			return false
		}
		return strconv.Itoa(int(typed)) == resource
	default:
		return false
	}
}

func known(site string) bool {
	switch site {
	case "open.cd", "dicmusic", "orpheus", "redacted":
		return true
	default:
		return false
	}
}

var errInput = inputError{}

type inputError struct{}

func (inputError) Error() string { return "free evidence request is invalid" }
