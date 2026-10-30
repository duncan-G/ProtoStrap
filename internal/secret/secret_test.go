package secret

import (
	"maps"
	"slices"
	"strings"
	"testing"
)

// all is an environment's secret: three components' keys, shared ones and ProtoStrap's.
func all() map[string]string {
	return map[string]string{
		"Api_ConnectionStrings__shop":     "Host=postgres;Password=api-pw",
		"Api_StripeKey":                   "sk_live_1",
		"Jobs_ConnectionStrings__shop":    "Host=postgres;Password=jobs-pw",
		"Shared_ConnectionStrings__redis": "redis:6379,password=r",
		"Shared_SentryDsn":                "https://sentry",
		"Postgres_POSTGRES_PASSWORD":      "pg-pw",
		"Edge_TUNNEL_TOKEN":               "t0k3n",
	}
}

func TestParse(t *testing.T) {
	got, err := Parse(`{"Api_X":"1","Shared_Y":"2"}`)
	if err != nil || !maps.Equal(got, map[string]string{"Api_X": "1", "Shared_Y": "2"}) {
		t.Errorf("parsed %v %v", got, err)
	}
	for _, bad := range []string{`not json`, `["Api_X"]`, `{"Api_X": 1}`, `{"Api_X": {"a": "b"}}`} {
		if _, err := Parse(bad); err == nil {
			t.Errorf("%s parsed", bad)
		}
	}
	if got, err := Parse(""); err != nil || len(got) != 0 {
		t.Errorf("a secret with no value: %v %v", got, err)
	}
}

func TestParseNeverQuotesAValue(t *testing.T) {
	_, err := Parse(`{"Api_X": 1, "Api_Y": "hunter2"`)
	if err == nil || strings.Contains(err.Error(), "hunter2") {
		t.Errorf("error %v", err)
	}
}

func TestKeysTakeThePrefixAndShared(t *testing.T) {
	got := Keys(all(), "Api_", []string{"ConnectionStrings__shop", "ConnectionStrings__redis"})
	want := map[string]string{
		"ConnectionStrings__shop":  "Host=postgres;Password=api-pw",
		"StripeKey":                "sk_live_1",
		"ConnectionStrings__redis": "redis:6379,password=r",
	}
	if !maps.Equal(got, want) {
		t.Errorf("keys %v\nwant %v", got, want)
	}
	if got := Keys(all(), Edge, nil); !maps.Equal(got, map[string]string{"TUNNEL_TOKEN": "t0k3n"}) {
		t.Errorf("edge keys %v", got)
	}
}

func TestOwnKeysWinOverShared(t *testing.T) {
	got := Keys(map[string]string{"Api_Db": "own", "Shared_Db": "shared"}, "Api_", []string{"Db"})
	if got["Db"] != "own" {
		t.Errorf("Db = %q", got["Db"])
	}
}

func TestAKeyThatIsOnlyAPrefixIsLeftOut(t *testing.T) {
	if got := Keys(map[string]string{"Api_": "x", "Shared_": "y"}, "Api_", []string{""}); len(got) != 0 {
		t.Errorf("keys %v", got)
	}
}

func TestMissing(t *testing.T) {
	own := Keys(all(), "Api_", []string{"ConnectionStrings__redis", "ConnectionStrings__other"})
	got := Missing(own, []string{"ConnectionStrings__shop", "ConnectionStrings__redis", "ConnectionStrings__other", "Nope"})
	if !slices.Equal(got, []string{"ConnectionStrings__other", "Nope"}) {
		t.Errorf("missing %v", got)
	}
}

func TestTheDigestCoversOnlyAComponentsKeys(t *testing.T) {
	key := []byte("k")
	names := []string{"ConnectionStrings__redis"}
	digest := func(change func(map[string]string)) string {
		a := all()
		change(a)
		return Digest(key, Keys(a, "Api_", names))
	}
	before := digest(func(map[string]string) {})
	if len(before) != 32 {
		t.Errorf("digest %q isn't 32 hex digits", before)
	}
	for name, tt := range map[string]struct {
		change func(map[string]string)
		same   bool
	}{
		"another component's key": {func(a map[string]string) { a["Jobs_ConnectionStrings__shop"] = "x" }, true},
		"ProtoStrap's key":        {func(a map[string]string) { a["Edge_TUNNEL_TOKEN"] = "x" }, true},
		"its own key":             {func(a map[string]string) { a["Api_StripeKey"] = "sk_live_2" }, false},
		"a new key of its own":    {func(a map[string]string) { a["Api_New"] = "x" }, false},
		"its own key removed":     {func(a map[string]string) { delete(a, "Api_StripeKey") }, false},
		"a shared key it names":   {func(a map[string]string) { a["Shared_ConnectionStrings__redis"] = "x" }, false},
	} {
		if got := digest(tt.change) == before; got != tt.same {
			t.Errorf("%s: digest unchanged = %v, want %v", name, got, tt.same)
		}
	}
}

func TestTheDigestIgnoresSharedKeysItDoesntList(t *testing.T) {
	key := []byte("k")
	a := all()
	before := Digest(key, Keys(a, "Api_", nil))
	a["Shared_SentryDsn"] = "https://other"
	if Digest(key, Keys(a, "Api_", nil)) != before {
		t.Errorf("a shared key Api doesn't list changed its digest")
	}
}

func TestTheDigestNeedsItsKey(t *testing.T) {
	own := map[string]string{"Password": "hunter2"}
	a, b := Digest([]byte("one"), own), Digest([]byte("two"), own)
	if a == b {
		t.Errorf("two keys gave one digest")
	}
	if strings.Contains(a, "hunter2") {
		t.Errorf("the digest holds the value")
	}
	if Digest([]byte("one"), map[string]string{"A": "bc"}) == Digest([]byte("one"), map[string]string{"Ab": "c"}) {
		t.Errorf("a key and value split differently gave one digest")
	}
}

func TestName(t *testing.T) {
	if got := Name("shop", "prod"); got != "shop/prod" {
		t.Errorf("name %q", got)
	}
}

func TestPrefixes(t *testing.T) {
	for _, p := range []string{"Api_", "DocumentImport_", "S3Sync_", "Edge_"} {
		if !ValidPrefix(p) {
			t.Errorf("%s should be valid", p)
		}
	}
	for _, p := range []string{"api_", "Api", "Api_V2_", "_", "9Api_", ""} {
		if ValidPrefix(p) {
			t.Errorf("%q should be invalid", p)
		}
	}
	for _, p := range []string{Shared, Edge, Reporter} {
		if !Reserved(p) {
			t.Errorf("%s should be reserved", p)
		}
	}
	if Reserved("Api_") {
		t.Errorf("Api_ isn't reserved")
	}
}
