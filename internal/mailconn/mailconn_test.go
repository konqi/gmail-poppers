package mailconn

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestCandidates(t *testing.T) {
	cases := []struct {
		mode string
		port int
		want []Endpoint
	}{
		{"auto", 0, []Endpoint{{"h", 995, Implicit}, {"h", 110, StartTLS}}},
		{"auto", 995, []Endpoint{{"h", 995, Implicit}}},
		{"auto", 110, []Endpoint{{"h", 110, StartTLS}}},
		{"auto", 2995, []Endpoint{{"h", 2995, Implicit}, {"h", 2995, StartTLS}}},
		{"implicit", 0, []Endpoint{{"h", 995, Implicit}}},
		{"starttls", 0, []Endpoint{{"h", 110, StartTLS}}},
		{"none", 0, []Endpoint{{"h", 110, Plain}}},
		{"none", 1110, []Endpoint{{"h", 1110, Plain}}},
	}
	for _, tc := range cases {
		got := Candidates(POP3, "h", tc.port, tc.mode)
		if !reflect.DeepEqual(got, tc.want) {
			t.Errorf("%s/%d: got %v, want %v", tc.mode, tc.port, got, tc.want)
		}
	}
	for _, mode := range []string{"auto", "implicit", "starttls"} {
		for _, port := range []int{0, 110, 143, 993, 995, 1234} {
			for _, e := range Candidates(IMAP, "h", port, mode) {
				if e.Security == Plain {
					t.Errorf("%s/%d: plaintext candidate", mode, port)
				}
			}
		}
	}
}

func TestConnect(t *testing.T) {
	cands := Candidates(IMAP, "h", 0, "auto")
	var tried []Security
	v, e, err := Connect(context.Background(), cands, func(e Endpoint) (int, error) {
		tried = append(tried, e.Security)
		if e.Security == Implicit {
			return 0, errors.New("refused")
		}
		return 42, nil
	})
	if err != nil || v != 42 || e.Port != 143 || len(tried) != 2 {
		t.Fatalf("v=%d e=%v err=%v tried=%v", v, e, err, tried)
	}

	_, _, err = Connect(context.Background(), cands, func(Endpoint) (int, error) { return 0, errors.New("nope") })
	if err == nil || !strings.Contains(err.Error(), "tls auto-detection failed") || !strings.Contains(err.Error(), "h:993") {
		t.Fatalf("err = %v", err)
	}
}
