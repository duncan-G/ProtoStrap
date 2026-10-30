package aws

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

type staticCreds struct{ c Credentials }

func (s staticCreds) Retrieve(context.Context) (Credentials, error) { return s.c, nil }

var testCreds = staticCreds{Credentials{AccessKeyID: "AK", SecretAccessKey: "SK", SessionToken: "ST"}}

// queryServer records each Query API call and answers it from handle.
type queryServer struct {
	mu     sync.Mutex
	calls  []url.Values
	handle func(form url.Values) (int, string)
}

func (q *queryServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "query APIs are POSTed", http.StatusMethodNotAllowed)
		return
	}
	auth := r.Header.Get("Authorization")
	if !strings.HasPrefix(auth, "AWS4-HMAC-SHA256 Credential=AK/") {
		http.Error(w, "unsigned: "+auth, http.StatusForbidden)
		return
	}
	if r.Header.Get("X-Amz-Security-Token") != "ST" {
		http.Error(w, "no session token", http.StatusForbidden)
		return
	}
	if ct := r.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/x-www-form-urlencoded") {
		http.Error(w, "content type "+ct, http.StatusBadRequest)
		return
	}
	r.ParseForm()
	q.mu.Lock()
	q.calls = append(q.calls, r.PostForm)
	q.mu.Unlock()
	status, body := q.handle(r.PostForm)
	w.WriteHeader(status)
	w.Write([]byte(body))
}

func (q *queryServer) only(t *testing.T) url.Values {
	t.Helper()
	q.mu.Lock()
	defer q.mu.Unlock()
	if len(q.calls) != 1 {
		t.Fatalf("expected one call, got %d", len(q.calls))
	}
	return q.calls[0]
}

func expectForm(t *testing.T, got url.Values, want map[string]string) {
	t.Helper()
	for k, v := range want {
		if got.Get(k) != v {
			t.Errorf("%s = %q, want %q (form %v)", k, got.Get(k), v, got)
		}
	}
}

func start(t *testing.T, q *queryServer) string {
	srv := httptest.NewServer(q)
	t.Cleanup(srv.Close)
	return srv.URL
}

func TestCompleteLifecycleAction(t *testing.T) {
	q := &queryServer{handle: func(url.Values) (int, string) {
		return 200, `<CompleteLifecycleActionResponse><CompleteLifecycleActionResult/></CompleteLifecycleActionResponse>`
	}}
	as := &AutoScaling{Endpoint: start(t, q), Region: "eu-west-1", Credentials: testCreds}
	if err := as.CompleteLifecycleAction(t.Context(), "shop-general", "launch", "i-1", "CONTINUE"); err != nil {
		t.Fatal(err)
	}
	expectForm(t, q.only(t), map[string]string{
		"Action": "CompleteLifecycleAction", "Version": "2011-01-01",
		"AutoScalingGroupName": "shop-general", "LifecycleHookName": "launch",
		"InstanceId": "i-1", "LifecycleActionResult": "CONTINUE",
	})
}

func TestRecordLifecycleActionHeartbeat(t *testing.T) {
	q := &queryServer{handle: func(url.Values) (int, string) { return 200, `<R/>` }}
	as := &AutoScaling{Endpoint: start(t, q), Region: "eu-west-1", Credentials: testCreds}
	if err := as.RecordLifecycleActionHeartbeat(t.Context(), "shop-compute", "terminate", "i-2"); err != nil {
		t.Fatal(err)
	}
	expectForm(t, q.only(t), map[string]string{
		"Action": "RecordLifecycleActionHeartbeat", "AutoScalingGroupName": "shop-compute",
		"LifecycleHookName": "terminate", "InstanceId": "i-2",
	})
}

func TestQueryErrorIsParsed(t *testing.T) {
	q := &queryServer{handle: func(url.Values) (int, string) {
		return 400, `<ErrorResponse><Error><Type>Sender</Type><Code>ValidationError</Code>` +
			`<Message>No active Lifecycle Action found with instance ID i-1</Message></Error></ErrorResponse>`
	}}
	as := &AutoScaling{Endpoint: start(t, q), Region: "eu-west-1", Credentials: testCreds}
	err := as.CompleteLifecycleAction(t.Context(), "g", "launch", "i-1", "CONTINUE")
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("want *APIError, got %T %v", err, err)
	}
	if apiErr.Code != "ValidationError" || apiErr.Status != 400 || !strings.Contains(apiErr.Message, "No active Lifecycle Action") {
		t.Fatalf("got %+v", apiErr)
	}
}

func TestAssumeRole(t *testing.T) {
	q := &queryServer{handle: func(url.Values) (int, string) {
		return 200, `<AssumeRoleResponse xmlns="https://sts.amazonaws.com/doc/2011-06-15/">
  <AssumeRoleResult>
    <Credentials>
      <AccessKeyId>ASIA1</AccessKeyId>
      <SecretAccessKey>secret</SecretAccessKey>
      <SessionToken>session</SessionToken>
      <Expiration>2026-10-24T13:00:00Z</Expiration>
    </Credentials>
  </AssumeRoleResult>
</AssumeRoleResponse>`
	}}
	sts := &STS{Endpoint: start(t, q), Region: "eu-west-1", Credentials: testCreds}
	c, err := sts.AssumeRole(t.Context(), "arn:aws:iam::1:role/protostrap-api", "api.i-1", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	want := Credentials{AccessKeyID: "ASIA1", SecretAccessKey: "secret", SessionToken: "session",
		Expires: time.Date(2026, 10, 24, 13, 0, 0, 0, time.UTC)}
	if c != want {
		t.Fatalf("got %+v", c)
	}
	expectForm(t, q.only(t), map[string]string{
		"Action": "AssumeRole", "Version": "2011-06-15", "RoleArn": "arn:aws:iam::1:role/protostrap-api",
		"RoleSessionName": "api.i-1", "DurationSeconds": "3600",
	})
}

func TestAssumeRoleSessionNameIsSanitised(t *testing.T) {
	q := &queryServer{handle: func(url.Values) (int, string) {
		return 200, `<AssumeRoleResponse><AssumeRoleResult><Credentials><AccessKeyId>a</AccessKeyId>` +
			`<SecretAccessKey>b</SecretAccessKey><SessionToken>c</SessionToken>` +
			`<Expiration>2026-10-24T13:00:00Z</Expiration></Credentials></AssumeRoleResult></AssumeRoleResponse>`
	}}
	sts := &STS{Endpoint: start(t, q), Region: "eu-west-1", Credentials: testCreds}
	long := strings.Repeat("x", 80) + " bad/chars"
	if _, err := sts.AssumeRole(t.Context(), "arn", long, time.Hour); err != nil {
		t.Fatal(err)
	}
	name := q.only(t).Get("RoleSessionName")
	if len(name) > 64 || strings.ContainsAny(name, " /") {
		t.Fatalf("session name %q breaks STS's rules", name)
	}
}

func TestEC2DescribeAddress(t *testing.T) {
	q := &queryServer{handle: func(url.Values) (int, string) {
		return 200, `<DescribeAddressesResponse xmlns="http://ec2.amazonaws.com/doc/2016-11-15/">
  <addressesSet><item><allocationId>eipalloc-1</allocationId><instanceId>i-old</instanceId></item></addressesSet>
</DescribeAddressesResponse>`
	}}
	ec2 := &EC2{Endpoint: start(t, q), Region: "eu-west-1", Credentials: testCreds}
	holder, err := ec2.AddressHolder(t.Context(), "eipalloc-1")
	if err != nil || holder != "i-old" {
		t.Fatalf("got %q %v", holder, err)
	}
	expectForm(t, q.only(t), map[string]string{"Action": "DescribeAddresses", "Version": "2016-11-15", "AllocationId.1": "eipalloc-1"})
}

func TestEC2DescribeUnassociatedAddress(t *testing.T) {
	q := &queryServer{handle: func(url.Values) (int, string) {
		return 200, `<DescribeAddressesResponse><addressesSet><item><allocationId>eipalloc-1</allocationId></item></addressesSet></DescribeAddressesResponse>`
	}}
	ec2 := &EC2{Endpoint: start(t, q), Region: "eu-west-1", Credentials: testCreds}
	holder, err := ec2.AddressHolder(t.Context(), "eipalloc-1")
	if err != nil || holder != "" {
		t.Fatalf("got %q %v", holder, err)
	}
}

func TestEC2DescribeInstance(t *testing.T) {
	q := &queryServer{handle: func(url.Values) (int, string) {
		return 200, `<DescribeInstancesResponse><reservationSet><item><instancesSet><item>
<instanceId>i-1</instanceId><launchTime>2026-10-24T07:00:00.000Z</launchTime><instanceState><code>16</code><name>running</name></instanceState>
</item></instancesSet></item></reservationSet></DescribeInstancesResponse>`
	}}
	ec2 := &EC2{Endpoint: start(t, q), Region: "eu-west-1", Credentials: testCreds}
	launched, state, err := ec2.Instance(t.Context(), "i-1")
	if err != nil {
		t.Fatal(err)
	}
	if !launched.Equal(time.Date(2026, 10, 24, 7, 0, 0, 0, time.UTC)) || state != "running" {
		t.Fatalf("got %v %q", launched, state)
	}
	expectForm(t, q.only(t), map[string]string{"Action": "DescribeInstances", "InstanceId.1": "i-1"})
}

func TestEC2DescribeMissingInstance(t *testing.T) {
	q := &queryServer{handle: func(url.Values) (int, string) {
		return 400, `<Response><Errors><Error><Code>InvalidInstanceID.NotFound</Code><Message>gone</Message></Error></Errors></Response>`
	}}
	ec2 := &EC2{Endpoint: start(t, q), Region: "eu-west-1", Credentials: testCreds}
	_, state, err := ec2.Instance(t.Context(), "i-gone")
	if err != nil || state != "terminated" {
		t.Fatalf("a missing instance is terminated, got %q %v", state, err)
	}
}

func TestEC2AssociateAddress(t *testing.T) {
	q := &queryServer{handle: func(url.Values) (int, string) { return 200, `<AssociateAddressResponse/>` }}
	ec2 := &EC2{Endpoint: start(t, q), Region: "eu-west-1", Credentials: testCreds}
	if err := ec2.AssociateAddress(t.Context(), "eipalloc-1", "i-new"); err != nil {
		t.Fatal(err)
	}
	expectForm(t, q.only(t), map[string]string{
		"Action": "AssociateAddress", "AllocationId": "eipalloc-1", "InstanceId": "i-new", "AllowReassociation": "true",
	})
}

func TestEC2ReplaceRoute(t *testing.T) {
	q := &queryServer{handle: func(url.Values) (int, string) { return 200, `<ReplaceRouteResponse/>` }}
	ec2 := &EC2{Endpoint: start(t, q), Region: "eu-west-1", Credentials: testCreds}
	if err := ec2.RouteVia(t.Context(), "rtb-1", "0.0.0.0/0", "i-new"); err != nil {
		t.Fatal(err)
	}
	expectForm(t, q.only(t), map[string]string{
		"Action": "ReplaceRoute", "RouteTableId": "rtb-1", "DestinationCidrBlock": "0.0.0.0/0", "InstanceId": "i-new",
	})
}

func TestEC2RouteIsCreatedWhenMissing(t *testing.T) {
	q := &queryServer{handle: func(f url.Values) (int, string) {
		if f.Get("Action") == "ReplaceRoute" {
			return 400, `<Response><Errors><Error><Code>InvalidRoute.NotFound</Code><Message>no route</Message></Error></Errors></Response>`
		}
		return 200, `<CreateRouteResponse/>`
	}}
	ec2 := &EC2{Endpoint: start(t, q), Region: "eu-west-1", Credentials: testCreds}
	if err := ec2.RouteVia(t.Context(), "rtb-1", "0.0.0.0/0", "i-new"); err != nil {
		t.Fatal(err)
	}
	if len(q.calls) != 2 || q.calls[1].Get("Action") != "CreateRoute" {
		t.Fatalf("expected ReplaceRoute then CreateRoute, got %v", q.calls)
	}
}

func TestEC2DisableSourceDestCheck(t *testing.T) {
	q := &queryServer{handle: func(url.Values) (int, string) { return 200, `<ModifyInstanceAttributeResponse/>` }}
	ec2 := &EC2{Endpoint: start(t, q), Region: "eu-west-1", Credentials: testCreds}
	if err := ec2.DisableSourceDestCheck(t.Context(), "i-new"); err != nil {
		t.Fatal(err)
	}
	expectForm(t, q.only(t), map[string]string{
		"Action": "ModifyInstanceAttribute", "InstanceId": "i-new", "SourceDestCheck.Value": "false",
	})
}

func TestEC2ErrorIsParsed(t *testing.T) {
	q := &queryServer{handle: func(url.Values) (int, string) {
		return 403, `<Response><Errors><Error><Code>UnauthorizedOperation</Code><Message>denied</Message></Error></Errors></Response>`
	}}
	ec2 := &EC2{Endpoint: start(t, q), Region: "eu-west-1", Credentials: testCreds}
	err := ec2.AssociateAddress(t.Context(), "a", "i")
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Code != "UnauthorizedOperation" {
		t.Fatalf("got %v", err)
	}
}

func TestDefaultEndpoints(t *testing.T) {
	if got := (&AutoScaling{Region: "eu-west-1"}).endpoint(); got != "https://autoscaling.eu-west-1.amazonaws.com" {
		t.Error(got)
	}
	if got := (&STS{Region: "eu-west-1"}).endpoint(); got != "https://sts.eu-west-1.amazonaws.com" {
		t.Error(got)
	}
	if got := (&EC2{Region: "eu-west-1"}).endpoint(); got != "https://ec2.eu-west-1.amazonaws.com" {
		t.Error(got)
	}
}
