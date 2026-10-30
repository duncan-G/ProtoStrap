package aws

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestDescribeGroupsByTag(t *testing.T) {
	q := &queryServer{handle: func(form url.Values) (int, string) {
		if form.Get("NextToken") == "" {
			return 200, `<DescribeAutoScalingGroupsResponse><DescribeAutoScalingGroupsResult>
				<AutoScalingGroups><member>
					<AutoScalingGroupName>protostrap-general-us-east-1a</AutoScalingGroupName>
					<MinSize>1</MinSize><MaxSize>7</MaxSize><DesiredCapacity>2</DesiredCapacity>
					<Instances>
						<member><InstanceId>i-1</InstanceId><LifecycleState>InService</LifecycleState><AvailabilityZone>us-east-1a</AvailabilityZone></member>
						<member><InstanceId>i-2</InstanceId><LifecycleState>Pending:Wait</LifecycleState><AvailabilityZone>us-east-1a</AvailabilityZone></member>
					</Instances>
					<Tags>
						<member><Key>protostrap:pool</Key><Value>general</Value></member>
						<member><Key>protostrap:zone</Key><Value>us-east-1a</Value></member>
						<member><Key>protostrap:capacity</Key><Value>on-demand</Value></member>
					</Tags>
				</member></AutoScalingGroups>
				<NextToken>more</NextToken>
			</DescribeAutoScalingGroupsResult></DescribeAutoScalingGroupsResponse>`
		}
		return 200, `<DescribeAutoScalingGroupsResponse><DescribeAutoScalingGroupsResult><AutoScalingGroups><member>
			<AutoScalingGroupName>protostrap-compute-spot-us-east-1a</AutoScalingGroupName>
			<MinSize>0</MinSize><MaxSize>7</MaxSize><DesiredCapacity>0</DesiredCapacity>
			<Tags><member><Key>protostrap:pool</Key><Value>compute</Value></member><member><Key>protostrap:capacity</Key><Value>spot</Value></member></Tags>
		</member></AutoScalingGroups></DescribeAutoScalingGroupsResult></DescribeAutoScalingGroupsResponse>`
	}}
	a := &AutoScaling{Endpoint: start(t, q), Region: "us-east-1", Credentials: testCreds}
	groups, err := a.Groups(context.Background(), "shop")
	if err != nil {
		t.Fatal(err)
	}
	if len(groups) != 2 {
		t.Fatalf("groups %+v", groups)
	}
	g := groups[0]
	if g.Name != "protostrap-general-us-east-1a" || g.Min != 1 || g.Max != 7 || g.Desired != 2 || g.Tags["protostrap:pool"] != "general" {
		t.Errorf("group %+v", g)
	}
	if len(g.Instances) != 2 || g.Instances[1].ID != "i-2" || g.Instances[1].State != "Pending:Wait" || g.Instances[0].Zone != "us-east-1a" {
		t.Errorf("instances %+v", g.Instances)
	}
	if groups[1].Tags["protostrap:capacity"] != "spot" {
		t.Errorf("second page %+v", groups[1])
	}
	first := q.calls[0]
	expectForm(t, first, map[string]string{
		"Action": "DescribeAutoScalingGroups", "Filters.member.1.Name": "tag:protostrap:app", "Filters.member.1.Values.member.1": "shop",
	})
	if q.calls[1].Get("NextToken") != "more" {
		t.Errorf("the second page wasn't asked for")
	}
}

func TestScalingCalls(t *testing.T) {
	q := &queryServer{handle: func(url.Values) (int, string) { return 200, "<Response/>" }}
	a := &AutoScaling{Endpoint: start(t, q), Region: "us-east-1", Credentials: testCreds}
	ctx := context.Background()
	if err := a.SetDesiredCapacity(ctx, "g", 3); err != nil {
		t.Fatal(err)
	}
	if err := a.TerminateInstance(ctx, "i-1", true); err != nil {
		t.Fatal(err)
	}
	if err := a.SetUnhealthy(ctx, "i-2"); err != nil {
		t.Fatal(err)
	}
	expectForm(t, q.calls[0], map[string]string{"Action": "SetDesiredCapacity", "AutoScalingGroupName": "g", "DesiredCapacity": "3", "HonorCooldown": "false"})
	expectForm(t, q.calls[1], map[string]string{"Action": "TerminateInstanceInAutoScalingGroup", "InstanceId": "i-1", "ShouldDecrementDesiredCapacity": "true"})
	expectForm(t, q.calls[2], map[string]string{"Action": "SetInstanceHealth", "InstanceId": "i-2", "HealthStatus": "Unhealthy", "ShouldRespectGracePeriod": "false"})
}

func TestQueueDepth(t *testing.T) {
	q := &queryServer{handle: func(url.Values) (int, string) {
		return 200, `<GetQueueAttributesResponse><GetQueueAttributesResult>
			<Attribute><Name>ApproximateNumberOfMessages</Name><Value>900</Value></Attribute>
			<Attribute><Name>ApproximateNumberOfMessagesNotVisible</Name><Value>60</Value></Attribute>
		</GetQueueAttributesResult></GetQueueAttributesResponse>`
	}}
	s := &SQS{Endpoint: start(t, q), Region: "us-east-1", Credentials: testCreds}
	visible, inFlight, err := s.Depth(context.Background(), "https://sqs.us-east-1.amazonaws.com/1/protostrap-jobs")
	if err != nil || visible != 900 || inFlight != 60 {
		t.Errorf("depth %d %d %v", visible, inFlight, err)
	}
	expectForm(t, q.only(t), map[string]string{
		"Action": "GetQueueAttributes", "QueueUrl": "https://sqs.us-east-1.amazonaws.com/1/protostrap-jobs",
		"AttributeName.1": "ApproximateNumberOfMessages", "AttributeName.2": "ApproximateNumberOfMessagesNotVisible",
	})
}

func TestInstancesByTag(t *testing.T) {
	q := &queryServer{handle: func(form url.Values) (int, string) {
		return 200, `<DescribeInstancesResponse><reservationSet>
			<item><instancesSet><item><instanceId>i-1</instanceId><privateIpAddress>10.20.1.10</privateIpAddress>
				<tagSet><item><key>protostrap:vm</key><value>data-1</value></item></tagSet></item></instancesSet></item>
			<item><instancesSet><item><instanceId>i-2</instanceId><privateIpAddress>10.20.16.5</privateIpAddress></item></instancesSet></item>
		</reservationSet></DescribeInstancesResponse>`
	}}
	e := &EC2{Endpoint: start(t, q), Region: "us-east-1", Credentials: testCreds}
	got, err := e.InstancesByTag(context.Background(), "protostrap:app", "shop")
	if err != nil || got["i-1"].PrivateIP != "10.20.1.10" || got["i-2"].PrivateIP != "10.20.16.5" || got["i-1"].Tags["protostrap:vm"] != "data-1" {
		t.Errorf("instances %+v %v", got, err)
	}
	expectForm(t, q.only(t), map[string]string{
		"Action": "DescribeInstances", "Filter.1.Name": "tag:protostrap:app", "Filter.1.Value.1": "shop",
		"Filter.2.Name": "instance-state-name", "Filter.2.Value.1": "pending", "Filter.2.Value.2": "running",
	})
}

// jsonServer answers AWS JSON-protocol calls by their target.
type jsonServer struct {
	t      *testing.T
	calls  []string
	bodies []map[string]any
	handle func(target string, body map[string]any) (int, string)
}

func (j *jsonServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 Credential=AK/") {
		http.Error(w, "unsigned", 403)
		return
	}
	if ct := r.Header.Get("Content-Type"); ct != "application/x-amz-json-1.1" {
		http.Error(w, "content type "+ct, 400)
		return
	}
	target := r.Header.Get("X-Amz-Target")
	b, _ := io.ReadAll(r.Body)
	var body map[string]any
	json.Unmarshal(b, &body)
	j.calls = append(j.calls, target)
	j.bodies = append(j.bodies, body)
	status, out := j.handle(target, body)
	w.WriteHeader(status)
	w.Write([]byte(out))
}

func TestGetSecretValue(t *testing.T) {
	j := &jsonServer{t: t, handle: func(target string, body map[string]any) (int, string) {
		if body["SecretId"] == "protostrap/missing" {
			return 400, `{"__type":"com.amazonaws.secretsmanager#ResourceNotFoundException","Message":"Secrets Manager can't find the specified secret value"}`
		}
		return 200, `{"SecretString":"{\"TUNNEL_TOKEN\":\"t0k3n\"}"}`
	}}
	srv := httptest.NewServer(j)
	defer srv.Close()
	sm := &SecretsManager{Endpoint: srv.URL, Region: "us-east-1", Credentials: testCreds}
	v, err := sm.GetSecretValue(context.Background(), "protostrap/edge")
	if err != nil || v != `{"TUNNEL_TOKEN":"t0k3n"}` {
		t.Errorf("secret %q %v", v, err)
	}
	if j.calls[0] != "secretsmanager.GetSecretValue" {
		t.Errorf("target %q", j.calls[0])
	}
	_, err = sm.GetSecretValue(context.Background(), "protostrap/missing")
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Code != "ResourceNotFoundException" || !errors.Is(err, ErrNotFound) {
		t.Errorf("err %v", err)
	}
}

func TestGetSecretValueVersion(t *testing.T) {
	j := &jsonServer{t: t, handle: func(target string, body map[string]any) (int, string) {
		version, _ := body["VersionId"].(string)
		if version == "" {
			version = "v-current"
		}
		return 200, `{"SecretString":"{\"Api_X\":\"1\"}","VersionId":"` + version + `"}`
	}}
	srv := httptest.NewServer(j)
	defer srv.Close()
	sm := &SecretsManager{Endpoint: srv.URL, Region: "us-east-1", Credentials: testCreds}
	v, id, err := sm.GetSecretValueVersion(context.Background(), "shop/prod", "v-1")
	if err != nil || v != `{"Api_X":"1"}` || id != "v-1" {
		t.Errorf("secret %q %q %v", v, id, err)
	}
	if j.bodies[0]["SecretId"] != "shop/prod" || j.bodies[0]["VersionId"] != "v-1" {
		t.Errorf("asked for %v", j.bodies[0])
	}
	_, id, err = sm.GetSecretValueVersion(context.Background(), "shop/prod", "")
	if err != nil || id != "v-current" {
		t.Errorf("current version %q %v", id, err)
	}
	if _, ok := j.bodies[1]["VersionId"]; ok {
		t.Errorf("asking for the current version sent a VersionId: %v", j.bodies[1])
	}
}

func TestDescribeSecret(t *testing.T) {
	j := &jsonServer{t: t, handle: func(target string, body map[string]any) (int, string) {
		switch body["SecretId"] {
		case "shop/prod":
			return 200, `{"Name":"shop/prod","VersionIdsToStages":{"v-2":["AWSCURRENT"],"v-1":["AWSPREVIOUS"],"v-0":[]}}`
		case "shop/empty":
			return 200, `{"Name":"shop/empty"}`
		}
		return 400, `{"__type":"ResourceNotFoundException","Message":"no such secret"}`
	}}
	srv := httptest.NewServer(j)
	defer srv.Close()
	sm := &SecretsManager{Endpoint: srv.URL, Region: "us-east-1", Credentials: testCreds}
	v, err := sm.CurrentVersion(context.Background(), "shop/prod")
	if err != nil || v != "v-2" {
		t.Errorf("current %q %v", v, err)
	}
	if j.calls[0] != "secretsmanager.DescribeSecret" || len(j.bodies[0]) != 1 {
		t.Errorf("called %s with %v", j.calls[0], j.bodies[0])
	}
	if v, err := sm.CurrentVersion(context.Background(), "shop/empty"); err != nil || v != "" {
		t.Errorf("a secret with no value: %q %v", v, err)
	}
	if _, err := sm.CurrentVersion(context.Background(), "shop/missing"); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing secret: %v", err)
	}
}

func TestLogs(t *testing.T) {
	j := &jsonServer{t: t, handle: func(target string, body map[string]any) (int, string) {
		switch target {
		case "Logs_20140328.CreateLogStream":
			return 400, `{"__type":"ResourceAlreadyExistsException","message":"exists"}`
		case "Logs_20140328.PutLogEvents":
			return 200, `{}`
		case "Logs_20140328.StartQuery":
			return 200, `{"queryId":"q-1"}`
		case "Logs_20140328.GetQueryResults":
			return 200, `{"status":"Complete","results":[[{"field":"@message","value":"hello"},{"field":"@timestamp","value":"2026-10-25"}]]}`
		}
		return 400, `{"__type":"Unknown"}`
	}}
	srv := httptest.NewServer(j)
	defer srv.Close()
	l := &Logs{Endpoint: srv.URL, Region: "us-east-1", Credentials: testCreds}
	ctx := context.Background()
	if err := l.CreateLogStream(ctx, "/protostrap/shop/scheduler", "data-1"); err != nil {
		t.Errorf("an existing stream is not an error: %v", err)
	}
	at := time.Date(2026, 10, 25, 12, 0, 0, 0, time.UTC)
	if err := l.PutLogEvents(ctx, "/protostrap/shop/scheduler", "data-1", []LogEvent{{Time: at, Message: "m"}}); err != nil {
		t.Fatal(err)
	}
	put := j.bodies[1]
	events := put["logEvents"].([]any)
	if put["logGroupName"] != "/protostrap/shop/scheduler" || events[0].(map[string]any)["timestamp"].(float64) != float64(at.UnixMilli()) {
		t.Errorf("put %v", put)
	}
	rows, err := l.Query(ctx, []string{"/protostrap/shop/scheduler"}, "fields @message", at.Add(-time.Hour), at, time.Millisecond)
	if err != nil || len(rows) != 1 || rows[0]["@message"] != "hello" {
		t.Errorf("rows %v %v", rows, err)
	}
}

func TestContainerCredentials(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/credentials/abc" {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(`{"AccessKeyId":"AK","SecretAccessKey":"SK","Token":"ST","Expiration":"2026-10-25T13:00:00Z"}`))
	}))
	defer srv.Close()
	p := &ContainerCredentials{Base: srv.URL, RelativeURI: "/v1/credentials/abc"}
	c, err := p.Retrieve(context.Background())
	if err != nil || c.AccessKeyID != "AK" || c.SessionToken != "ST" || c.Expires.Hour() != 13 {
		t.Errorf("credentials %+v %v", c, err)
	}
}

func TestS3Put(t *testing.T) {
	f := newFakeS3()
	s := newS3(t, f)
	if err := s.Put(context.Background(), "bucket", "state.json", []byte(`{"a":1}`)); err != nil {
		t.Fatal(err)
	}
	if got := string(f.objects["bucket/state.json"]); got != `{"a":1}` {
		t.Errorf("stored %q", got)
	}
}

func TestLogGroups(t *testing.T) {
	j := &jsonServer{t: t, handle: func(target string, body map[string]any) (int, string) {
		if target != "Logs_20140328.DescribeLogGroups" || body["logGroupNamePrefix"] != "/protostrap/shop/" {
			return 400, `{"__type":"Unknown"}`
		}
		if body["nextToken"] == nil {
			return 200, `{"logGroups":[{"logGroupName":"/protostrap/shop/api"},{"logGroupName":"/protostrap/shop/scheduler"}],"nextToken":"p2"}`
		}
		return 200, `{"logGroups":[{"logGroupName":"/protostrap/shop/web"}]}`
	}}
	srv := httptest.NewServer(j)
	defer srv.Close()
	l := &Logs{Endpoint: srv.URL, Region: "us-east-1", Credentials: testCreds}
	groups, err := l.Groups(context.Background(), "/protostrap/shop/")
	if err != nil || strings.Join(groups, ",") != "/protostrap/shop/api,/protostrap/shop/scheduler,/protostrap/shop/web" {
		t.Errorf("groups %v %v: every page", groups, err)
	}
}
