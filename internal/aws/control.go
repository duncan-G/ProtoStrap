package aws

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"
)

// This file holds what the scheduler, the reporter and the edge call besides the agent's
// calls: Auto Scaling groups, queue depths, the application's instances, CloudWatch Logs,
// Secrets Manager, and container credentials.

// GroupInfo is an Auto Scaling group.
type GroupInfo struct {
	Name      string
	Min, Max  int
	Desired   int
	Instances []GroupInstance
	Tags      map[string]string
}

// GroupInstance is an instance in a group.
type GroupInstance struct {
	ID string
	// State is the lifecycle state, such as Pending, Pending:Wait, InService or Terminating.
	State string
	Zone  string
}

// Groups describes the groups tagged protostrap:app=<app>.
func (a *AutoScaling) Groups(ctx context.Context, app string) ([]GroupInfo, error) {
	var out []GroupInfo
	token := ""
	for {
		params := url.Values{
			"Filters.member.1.Name":            {"tag:protostrap:app"},
			"Filters.member.1.Values.member.1": {app},
			"MaxRecords":                       {"100"},
		}
		if token != "" {
			params.Set("NextToken", token)
		}
		var page struct {
			Groups []struct {
				Name      string `xml:"AutoScalingGroupName"`
				Min       int    `xml:"MinSize"`
				Max       int    `xml:"MaxSize"`
				Desired   int    `xml:"DesiredCapacity"`
				Instances []struct {
					ID    string `xml:"InstanceId"`
					State string `xml:"LifecycleState"`
					Zone  string `xml:"AvailabilityZone"`
				} `xml:"Instances>member"`
				Tags []struct {
					Key   string `xml:"Key"`
					Value string `xml:"Value"`
				} `xml:"Tags>member"`
			} `xml:"DescribeAutoScalingGroupsResult>AutoScalingGroups>member"`
			NextToken string `xml:"DescribeAutoScalingGroupsResult>NextToken"`
		}
		if err := a.client().call(ctx, "DescribeAutoScalingGroups", params, &page); err != nil {
			return nil, err
		}
		for _, g := range page.Groups {
			info := GroupInfo{Name: g.Name, Min: g.Min, Max: g.Max, Desired: g.Desired, Tags: map[string]string{}}
			for _, in := range g.Instances {
				info.Instances = append(info.Instances, GroupInstance{ID: in.ID, State: in.State, Zone: in.Zone})
			}
			for _, t := range g.Tags {
				info.Tags[t.Key] = t.Value
			}
			out = append(out, info)
		}
		if page.NextToken == "" {
			return out, nil
		}
		token = page.NextToken
	}
}

// SetDesiredCapacity sets a group's desired capacity.
func (a *AutoScaling) SetDesiredCapacity(ctx context.Context, group string, desired int) error {
	return a.client().call(ctx, "SetDesiredCapacity", url.Values{
		"AutoScalingGroupName": {group}, "DesiredCapacity": {strconv.Itoa(desired)}, "HonorCooldown": {"false"},
	}, nil)
}

// TerminateInstance terminates an instance through its group, lowering the group's desired
// capacity in the same call when decrement is set, so the group doesn't replace it.
func (a *AutoScaling) TerminateInstance(ctx context.Context, instance string, decrement bool) error {
	return a.client().call(ctx, "TerminateInstanceInAutoScalingGroup", url.Values{
		"InstanceId": {instance}, "ShouldDecrementDesiredCapacity": {strconv.FormatBool(decrement)},
	}, nil)
}

// SetUnhealthy marks an instance unhealthy, so its group replaces it.
func (a *AutoScaling) SetUnhealthy(ctx context.Context, instance string) error {
	return a.client().call(ctx, "SetInstanceHealth", url.Values{
		"InstanceId": {instance}, "HealthStatus": {"Unhealthy"}, "ShouldRespectGracePeriod": {"false"},
	}, nil)
}

// SQS reads queue depths.
type SQS struct {
	Endpoint    string // It defaults to https://sqs.<region>.amazonaws.com.
	Region      string
	Credentials Provider
	HTTP        *http.Client
}

func (s *SQS) client() queryClient {
	endpoint := s.Endpoint
	if endpoint == "" {
		endpoint = "https://sqs." + s.Region + ".amazonaws.com"
	}
	return queryClient{endpoint: endpoint, region: s.Region, service: "sqs", version: "2012-11-05", creds: s.Credentials, http: s.HTTP}
}

// Depth returns how many of a queue's jobs are waiting and how many are in flight
// (received but not yet deleted).
func (s *SQS) Depth(ctx context.Context, queueURL string) (visible, inFlight int, err error) {
	var out struct {
		Attributes []struct {
			Name  string `xml:"Name"`
			Value string `xml:"Value"`
		} `xml:"GetQueueAttributesResult>Attribute"`
	}
	err = s.client().call(ctx, "GetQueueAttributes", url.Values{
		"QueueUrl":        {queueURL},
		"AttributeName.1": {"ApproximateNumberOfMessages"},
		"AttributeName.2": {"ApproximateNumberOfMessagesNotVisible"},
	}, &out)
	if err != nil {
		return 0, 0, err
	}
	for _, a := range out.Attributes {
		n, _ := strconv.Atoi(a.Value)
		switch a.Name {
		case "ApproximateNumberOfMessages":
			visible = n
		case "ApproximateNumberOfMessagesNotVisible":
			inFlight = n
		}
	}
	return visible, inFlight, nil
}

// InstanceInfo is a running instance.
type InstanceInfo struct {
	PrivateIP string
	Tags      map[string]string
}

// InstancesByTag lists the pending and running instances with a tag, keyed by instance ID.
func (e *EC2) InstancesByTag(ctx context.Context, key, value string) (map[string]InstanceInfo, error) {
	out := map[string]InstanceInfo{}
	token := ""
	for {
		params := url.Values{
			"Filter.1.Name": {"tag:" + key}, "Filter.1.Value.1": {value},
			"Filter.2.Name": {"instance-state-name"}, "Filter.2.Value.1": {"pending"}, "Filter.2.Value.2": {"running"},
		}
		if token != "" {
			params.Set("NextToken", token)
		}
		var page struct {
			Instances []struct {
				ID   string `xml:"instanceId"`
				IP   string `xml:"privateIpAddress"`
				Tags []struct {
					Key   string `xml:"key"`
					Value string `xml:"value"`
				} `xml:"tagSet>item"`
			} `xml:"reservationSet>item>instancesSet>item"`
			NextToken string `xml:"nextToken"`
		}
		if err := e.client().call(ctx, "DescribeInstances", params, &page); err != nil {
			return nil, err
		}
		for _, in := range page.Instances {
			info := InstanceInfo{PrivateIP: in.IP, Tags: map[string]string{}}
			for _, t := range in.Tags {
				info.Tags[t.Key] = t.Value
			}
			out[in.ID] = info
		}
		if page.NextToken == "" {
			return out, nil
		}
		token = page.NextToken
	}
}

// Put writes a small object in one request.
func (s *S3) Put(ctx context.Context, bucket, key string, body []byte) error {
	return s.put(ctx, bucket, key, body)
}

// jsonClient calls an AWS JSON-protocol API: a signed POST of JSON, with the operation in
// X-Amz-Target.
type jsonClient struct {
	endpoint string
	region   string
	service  string
	target   string // It is the X-Amz-Target prefix, such as Logs_20140328.
	creds    Provider
	http     *http.Client
}

func (j jsonClient) call(ctx context.Context, op string, in, out any) error {
	body, err := json.Marshal(in)
	if err != nil {
		return err
	}
	creds, err := j.creds.Retrieve(ctx)
	if err != nil {
		return fmt.Errorf("%s %s: credentials: %w", j.service, op, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, j.endpoint+"/", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-amz-json-1.1")
	req.Header.Set("X-Amz-Target", j.target+"."+op)
	Sign(req, HashHex(body), creds, j.region, j.service, time.Now())
	client := j.http
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", j.service, op, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return fmt.Errorf("%s %s: %w", j.service, op, err)
	}
	if resp.StatusCode >= 300 {
		var e struct {
			Type     string `json:"__type"`
			Message  string `json:"message"`
			Message2 string `json:"Message"`
		}
		json.Unmarshal(data, &e)
		code := e.Type[strings.LastIndex(e.Type, "#")+1:]
		if code == "" {
			code = http.StatusText(resp.StatusCode)
		}
		return &APIError{Status: resp.StatusCode, Code: code, Message: firstOf(e.Message, e.Message2)}
	}
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("%s %s: decoding the answer: %w", j.service, op, err)
		}
	}
	return nil
}

// SecretsManager reads secrets.
type SecretsManager struct {
	Endpoint    string // It defaults to https://secretsmanager.<region>.amazonaws.com.
	Region      string
	Credentials Provider
	HTTP        *http.Client
}

func (s *SecretsManager) client() jsonClient {
	endpoint := s.Endpoint
	if endpoint == "" {
		endpoint = "https://secretsmanager." + s.Region + ".amazonaws.com"
	}
	return jsonClient{endpoint: endpoint, region: s.Region, service: "secretsmanager", target: "secretsmanager", creds: s.Credentials, http: s.HTTP}
}

// GetSecretValue returns a secret's current string value.
func (s *SecretsManager) GetSecretValue(ctx context.Context, id string) (string, error) {
	value, _, err := s.GetSecretValueVersion(ctx, id, "")
	return value, err
}

// GetSecretValueVersion returns a version of a secret's string value, and its id. An empty
// version is the current one.
func (s *SecretsManager) GetSecretValueVersion(ctx context.Context, id, version string) (value, versionID string, err error) {
	in := map[string]string{"SecretId": id}
	if version != "" {
		in["VersionId"] = version
	}
	var out struct {
		SecretString string `json:"SecretString"`
		VersionID    string `json:"VersionId"`
	}
	if err := s.client().call(ctx, "GetSecretValue", in, &out); err != nil {
		return "", "", err
	}
	return out.SecretString, out.VersionID, nil
}

// CurrentVersion returns the id of a secret's current version, or "" if it has no value
// yet. It reads the secret's metadata, never its value.
func (s *SecretsManager) CurrentVersion(ctx context.Context, id string) (string, error) {
	var out struct {
		VersionIdsToStages map[string][]string `json:"VersionIdsToStages"`
	}
	if err := s.client().call(ctx, "DescribeSecret", map[string]string{"SecretId": id}, &out); err != nil {
		return "", err
	}
	for version, stages := range out.VersionIdsToStages {
		if slices.Contains(stages, "AWSCURRENT") {
			return version, nil
		}
	}
	return "", nil
}

// Logs writes to and queries CloudWatch Logs.
type Logs struct {
	Endpoint    string // It defaults to https://logs.<region>.amazonaws.com.
	Region      string
	Credentials Provider
	HTTP        *http.Client
}

func (l *Logs) client() jsonClient {
	endpoint := l.Endpoint
	if endpoint == "" {
		endpoint = "https://logs." + l.Region + ".amazonaws.com"
	}
	return jsonClient{endpoint: endpoint, region: l.Region, service: "logs", target: "Logs_20140328", creds: l.Credentials, http: l.HTTP}
}

// LogEvent is one log line.
type LogEvent struct {
	Time    time.Time
	Message string
}

// CreateLogStream makes a stream; one that exists already is not an error.
func (l *Logs) CreateLogStream(ctx context.Context, group, stream string) error {
	err := l.client().call(ctx, "CreateLogStream", map[string]string{"logGroupName": group, "logStreamName": stream}, nil)
	if errorCode(err) == "ResourceAlreadyExistsException" {
		return nil
	}
	return err
}

// PutLogEvents writes events, which must be in time order.
func (l *Logs) PutLogEvents(ctx context.Context, group, stream string, events []LogEvent) error {
	type event struct {
		Timestamp int64  `json:"timestamp"`
		Message   string `json:"message"`
	}
	in := struct {
		Group  string  `json:"logGroupName"`
		Stream string  `json:"logStreamName"`
		Events []event `json:"logEvents"`
	}{Group: group, Stream: stream}
	for _, e := range events {
		in.Events = append(in.Events, event{e.Time.UnixMilli(), e.Message})
	}
	return l.client().call(ctx, "PutLogEvents", in, nil)
}

// Groups lists the log groups whose names start with prefix.
func (l *Logs) Groups(ctx context.Context, prefix string) ([]string, error) {
	var names []string
	in := map[string]any{"logGroupNamePrefix": prefix}
	for {
		var out struct {
			LogGroups []struct {
				Name string `json:"logGroupName"`
			} `json:"logGroups"`
			NextToken string `json:"nextToken"`
		}
		if err := l.client().call(ctx, "DescribeLogGroups", in, &out); err != nil {
			return nil, err
		}
		for _, g := range out.LogGroups {
			names = append(names, g.Name)
		}
		if out.NextToken == "" {
			return names, nil
		}
		in["nextToken"] = out.NextToken
	}
}

// Query runs a Logs Insights query over groups and waits for its rows, each a map of field
// to value.
func (l *Logs) Query(ctx context.Context, groups []string, query string, from, to time.Time, poll time.Duration) ([]map[string]string, error) {
	var started struct {
		QueryID string `json:"queryId"`
	}
	err := l.client().call(ctx, "StartQuery", map[string]any{
		"logGroupNames": groups, "queryString": query, "startTime": from.Unix(), "endTime": to.Unix(), "limit": 10000,
	}, &started)
	if err != nil {
		return nil, err
	}
	for {
		var res struct {
			Status  string `json:"status"`
			Results [][]struct {
				Field string `json:"field"`
				Value string `json:"value"`
			} `json:"results"`
		}
		if err := l.client().call(ctx, "GetQueryResults", map[string]string{"queryId": started.QueryID}, &res); err != nil {
			return nil, err
		}
		switch res.Status {
		case "Complete":
			rows := make([]map[string]string, 0, len(res.Results))
			for _, r := range res.Results {
				row := map[string]string{}
				for _, f := range r {
					row[f.Field] = f.Value
				}
				rows = append(rows, row)
			}
			return rows, nil
		case "Failed", "Cancelled", "Timeout":
			return nil, fmt.Errorf("logs query %s", strings.ToLower(res.Status))
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(poll):
		}
	}
}

// ContainerCredentials are the credentials the agent serves a replica, at
// 169.254.170.2 plus AWS_CONTAINER_CREDENTIALS_RELATIVE_URI, as Amazon ECS does.
type ContainerCredentials struct {
	Base        string // It defaults to http://169.254.170.2.
	RelativeURI string // It defaults to $AWS_CONTAINER_CREDENTIALS_RELATIVE_URI.
	HTTP        *http.Client
}

func (c *ContainerCredentials) Retrieve(ctx context.Context) (Credentials, error) {
	base, uri := c.Base, c.RelativeURI
	if base == "" {
		base = "http://169.254.170.2"
	}
	if uri == "" {
		uri = os.Getenv("AWS_CONTAINER_CREDENTIALS_RELATIVE_URI")
	}
	if uri == "" {
		return Credentials{}, fmt.Errorf("no container credentials: AWS_CONTAINER_CREDENTIALS_RELATIVE_URI isn't set")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+uri, nil)
	if err != nil {
		return Credentials{}, err
	}
	client := c.HTTP
	if client == nil {
		client = &http.Client{Timeout: 5 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return Credentials{}, fmt.Errorf("container credentials: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Credentials{}, fmt.Errorf("container credentials: HTTP %d", resp.StatusCode)
	}
	var out struct {
		AccessKeyID     string    `json:"AccessKeyId"`
		SecretAccessKey string    `json:"SecretAccessKey"`
		Token           string    `json:"Token"`
		Expiration      time.Time `json:"Expiration"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return Credentials{}, fmt.Errorf("container credentials: %w", err)
	}
	return Credentials{AccessKeyID: out.AccessKeyID, SecretAccessKey: out.SecretAccessKey, SessionToken: out.Token, Expires: out.Expiration}, nil
}
