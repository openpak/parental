package profile

import (
	"strings"
	"testing"
)

func TestFullProfileRoundTrips(t *testing.T) {
	doc := `{"rating_age":12,"play_time":{"mon":60,"sun":180},"bedtime":{"start":"21:00","end":"07:00"},
	"titles":{"allow":[{"namespace":"switch","title_id":"0100000000010000"}]},
	"communication":{"mode":"friends_only","friend_requests":"blocked","ugc_posting":false,"free_communication":"restricted"},
	"store":{"purchases":"blocked","spending_limit":{"amount_minor":2000,"currency":"EUR","period":"month"}}}`
	p, canon, err := Parse([]byte(doc))
	if err != nil {
		t.Fatal(err)
	}
	if *p.RatingAge != 12 || p.PlayTime["sun"] != 180 || *p.Communication.UGCPosting {
		t.Fatalf("parsed %+v", p)
	}
	if _, again, err := Parse(canon); err != nil || string(again) != string(canon) {
		t.Fatalf("canonical form is not stable: %s vs %s (%v)", canon, again, err)
	}
}

func TestEmptyProfileIsUnrestricted(t *testing.T) {
	_, canon, err := Parse([]byte(`{}`))
	if err != nil || string(canon) != "{}" {
		t.Fatalf("got %s %v", canon, err)
	}
}

func TestRefusals(t *testing.T) {
	for doc, want := range map[string]string{
		`{"rating_age":22}`:                                                                 "rating_age",
		`{"play_time":{"monday":10}}`:                                                       "play_time.monday",
		`{"play_time":{"mon":1441}}`:                                                        "play_time.mon",
		`{"bedtime":{"start":"25:00","end":"07:00"}}`:                                       "bedtime.start",
		`{"titles":{"block":[{"namespace":"Switch","title_id":"1"}]}}`:                      "titles.block[0].namespace",
		`{"titles":{"allow":[{"namespace":"switch","title_id":""}]}}`:                       "titles.allow[0].title_id",
		`{"communication":{"mode":"everyone"}}`:                                             "communication.mode",
		`{"store":{"spending_limit":{"amount_minor":1,"currency":"eur","period":"month"}}}`: "currency",
		`{"store":{"spending_limit":{"amount_minor":1,"currency":"EUR"}}}`:                  "period",
		`{"pin":"1234"}`: "unknown field",
		`{} {}`:          "trailing",
		`[]`:             "profile",
	} {
		if _, _, err := Parse([]byte(doc)); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err %v, want it to name %q", doc, err, want)
		}
	}
}

func TestTooManyTitles(t *testing.T) {
	var b strings.Builder
	b.WriteString(`{"titles":{"allow":[`)
	for i := 0; i <= MaxTitles; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		b.WriteString(`{"namespace":"switch","title_id":"t"}`)
	}
	b.WriteString(`]}}`)
	if _, _, err := Parse([]byte(b.String())); err == nil {
		t.Fatal("501 titles accepted")
	}
}

func TestOverride(t *testing.T) {
	if _, err := ParseOverride([]byte(`{"restrictionMode":"ALARM"}`)); err != nil {
		t.Fatal(err)
	}
	for _, doc := range []string{`[]`, `"x"`, `null`, strings.Repeat(" ", MaxOverrideBytes+1)} {
		if _, err := ParseOverride([]byte(doc)); err == nil {
			t.Errorf("%.20q accepted", doc)
		}
	}
	if !ValidNamespace("switch") || ValidNamespace("") || ValidNamespace("Switch") {
		t.Fatal("namespace check")
	}
}
