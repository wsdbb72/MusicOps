package sitefree

import (
	"testing"
	"time"
)

func TestPersonalRowAuthorizesOnlyTheExactTorrent(t *testing.T) {
	page := `<tr id="torrent3"><td><a href="torrents.php?action=download&amp;id=3">DL</a><strong class="torrent_label tooltip tl_free" title="个人免费！">个人免费！</strong></td></tr>`
	decision, err := Read("dicmusic", "3", [][]byte{[]byte(page)}, time.Unix(10, 0))
	if err != nil || decision.Mode != "direct" || !decision.Personal || decision.TokenCost != 0 {
		t.Fatal(decision, err)
	}
	for _, page := range []string{
		`<tr id="torrent4"><strong class="tl_free">个人免费！</strong></tr><a href="torrents.php?action=download&amp;id=3">DL</a>`,
		`<tr id="torrent3"><strong class="tl_free">50% Free</strong></tr><a href="torrents.php?action=download&amp;id=3">DL</a>`,
		`<tr id="torrent3"><td>个人免费！</td></tr><a href="torrents.php?action=download&amp;id=3">DL</a>`,
		page + page,
	} {
		decision, err = Read("dicmusic", "3", [][]byte{[]byte(page)}, time.Unix(10, 0))
		if err != nil || decision.Mode != "" {
			t.Fatal(page, decision, err)
		}
	}
}

func TestTokenButtonRequiresOneCanonicalLinkAndABalance(t *testing.T) {
	page := `<a href="torrents.php?action=download&amp;id=3">DL</a><a href="torrents.php?action=download&amp;id=3&amp;usetoken=1">FL</a><span>令牌: 4</span>`
	decision, err := Read("orpheus", "3", [][]byte{[]byte(page)}, time.Unix(10, 0))
	if err != nil || decision.Mode != "token" || decision.TokenCost != 1 || decision.Balance == nil || *decision.Balance != 4 {
		t.Fatal(decision, err)
	}
	priced := `<a href="torrents.php?action=download&amp;id=3&amp;usetoken=1" title="Use 3 tokens">FL</a><span>令牌: 5</span>`
	decision, err = Read("dicmusic", "3", [][]byte{[]byte(priced)}, time.Unix(10, 0))
	if err != nil || decision.TokenCost != 3 || decision.Mode != "token" {
		t.Fatal(decision, err)
	}
	for _, token := range []string{"", "0", "false", "1&amp;useToken=1"} {
		page := `<a href="torrents.php?action=download&amp;id=3&amp;usetoken=` + token + `">FL</a><span>令牌: 4</span>`
		decision, err = Read("dicmusic", "3", [][]byte{[]byte(page)}, time.Unix(10, 0))
		if err != nil || decision.Mode != "" {
			t.Fatal(token, decision, err)
		}
	}
}

func TestGlobalFreeDoesNotSpendDicMusicTokens(t *testing.T) {
	body := []byte(`{"status":"success","response":{"torrent":{"id":"11","isFreeleech":true,"isPersonalFreeleech":false}}}`)
	decision, err := Read("dicmusic", "11", [][]byte{body}, time.Unix(10, 0))
	if err != nil || decision.Mode != "" || !decision.Global || decision.Personal {
		t.Fatal(decision, err)
	}
	// Redacted shares the token architecture: a global free torrent needs no
	// token, and the acquire path fetches it directly before mode is consulted.
	decision, err = Read("redacted", "11", [][]byte{body}, time.Unix(10, 0))
	if err != nil || decision.Mode != "" || !decision.Global {
		t.Fatal(decision, err)
	}
	// The same page with a token button is a token decision for redacted.
	priced := []byte(`<a href="torrents.php?action=download&amp;id=11&amp;usetoken=1">Use 1 token</a><span>Tokens: 3</span>`)
	decision, err = Read("redacted", "11", [][]byte{priced}, time.Unix(10, 0))
	if err != nil || decision.Mode != "token" || decision.TokenCost != 1 || decision.Balance == nil || *decision.Balance != 3 {
		t.Fatal(decision, err)
	}
}

func TestTorrentIdentityMustMatch(t *testing.T) {
	body := []byte(`{"status":"success","response":{"group":{"id":10},"torrent":{"id":99,"isPersonalFreeleech":true}}}`)
	decision, err := Read("dicmusic", "11", [][]byte{body}, time.Unix(10, 0))
	if err != nil || decision.Personal || decision.Mode != "" || GroupID(body, "11") != "" {
		t.Fatal(decision, err, GroupID(body, "11"))
	}
	matched := []byte(`{"status":"success","response":{"group":{"id":10},"torrent":{"id":11,"isPersonalFreeleech":true}}}`)
	if GroupID(matched, "11") != "10" {
		t.Fatal(GroupID(matched, "11"))
	}
}

func TestPageTokenTotalSumsAndRejectsAPartialNumber(t *testing.T) {
	summed := `<span>令牌: 0+1</span><a href="torrents.php?action=download&amp;id=3&amp;usetoken=1">FL</a>`
	decision, err := Read("dicmusic", "3", [][]byte{[]byte(summed)}, time.Unix(10, 0))
	if err != nil || decision.Balance == nil || *decision.Balance != 1 || decision.TokenCost != 1 {
		t.Fatal(decision, err)
	}
	grouped := `<span>Tokens: 1,000</span><a href="torrents.php?action=download&amp;id=3&amp;usetoken=1">FL</a>`
	decision, err = Read("orpheus", "3", [][]byte{[]byte(grouped)}, time.Unix(10, 0))
	if err != nil || decision.Balance == nil || *decision.Balance != 1000 {
		t.Fatal(decision, err)
	}
	split := `<span>令牌: 1</span><span>令牌: 2</span>`
	decision, err = Read("dicmusic", "3", [][]byte{[]byte(split)}, time.Unix(10, 0))
	if err != nil || decision.Balance != nil {
		t.Fatal(decision, err)
	}
	fraction := []byte(`{"status":"success","response":{"torrent":{"id":"3"},"personal":{"freeleechTokens":1.5}}}`)
	decision, err = Read("dicmusic", "3", [][]byte{fraction}, time.Unix(10, 0))
	if err != nil || decision.Balance != nil {
		t.Fatal(decision, err)
	}
}

func TestCallerClockIsRequired(t *testing.T) {
	if _, err := Read("open.cd", "11", [][]byte{[]byte(`<tr id="torrent11"><strong class="tl_free">免费</strong></tr>`)}, time.Time{}); err == nil {
		t.Fatal("missing observation time accepted")
	}
}
