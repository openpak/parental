package grpcapi

import "testing"

func TestParseKeys(t *testing.T) {
	k, err := ParseKeys(" switch = 0123456789abcdef0123456789abcdef ,wiiu=fedcba9876543210fedcba9876543210,")
	if err != nil || len(k) != 2 || k["switch"] != "0123456789abcdef0123456789abcdef" {
		t.Fatalf("%v %v", k, err)
	}
	for _, bad := range []string{
		"",
		"switch=short",
		"=0123456789abcdef0123456789abcdef",
		"Switch=0123456789abcdef0123456789abcdef",
		"switch=0123456789abcdef0123456789abcdef,switch=fedcba9876543210fedcba9876543210",
		"switch0123456789abcdef0123456789abcdef",
	} {
		if _, err := ParseKeys(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}
