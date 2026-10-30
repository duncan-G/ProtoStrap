package aws

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// queryClient calls an AWS Query API: a signed form POST answered with XML.
type queryClient struct {
	endpoint string
	region   string
	service  string
	version  string
	creds    Provider
	http     *http.Client
}

func (q queryClient) call(ctx context.Context, action string, params url.Values, out any) error {
	form := url.Values{}
	for k, v := range params {
		form[k] = v
	}
	form.Set("Action", action)
	form.Set("Version", q.version)
	body := form.Encode()

	creds, err := q.creds.Retrieve(ctx)
	if err != nil {
		return fmt.Errorf("%s %s: credentials: %w", q.service, action, err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, q.endpoint+"/", strings.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded; charset=utf-8")
	Sign(req, HashHex([]byte(body)), creds, q.region, q.service, time.Now())

	client := q.http
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", q.service, action, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return fmt.Errorf("%s %s: %w", q.service, action, err)
	}
	if resp.StatusCode >= 300 {
		return parseError(resp.StatusCode, data)
	}
	if out != nil {
		if err := xml.Unmarshal(data, out); err != nil {
			return fmt.Errorf("%s %s: decoding the answer: %w", q.service, action, err)
		}
	}
	return nil
}

// parseError reads the error formats of the Query APIs, EC2 and S3.
func parseError(status int, body []byte) error {
	var e struct {
		// S3 answers <Error><Code> and <Error><Message>.
		Code    string `xml:"Code"`
		Message string `xml:"Message"`
		// The Query APIs answer <ErrorResponse><Error><Code> and <Message>.
		QueryCode string `xml:"Error>Code"`
		QueryMsg  string `xml:"Error>Message"`
		// EC2 answers <Response><Errors><Error><Code> and <Message>.
		EC2Code    string `xml:"Errors>Error>Code"`
		EC2Message string `xml:"Errors>Error>Message"`
	}
	xml.Unmarshal(body, &e)
	apiErr := &APIError{Status: status, Code: firstOf(e.QueryCode, e.EC2Code, e.Code), Message: firstOf(e.QueryMsg, e.EC2Message, e.Message)}
	if apiErr.Code == "" {
		apiErr.Code = http.StatusText(status)
	}
	return apiErr
}

func firstOf(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func errorCode(err error) string {
	var apiErr *APIError
	if errors.As(err, &apiErr) {
		return apiErr.Code
	}
	return ""
}

// AutoScaling completes the lifecycle hooks that hold a VM while the agent readies or
// drains it.
type AutoScaling struct {
	Endpoint    string // It defaults to https://autoscaling.<region>.amazonaws.com.
	Region      string
	Credentials Provider
	HTTP        *http.Client
}

func (a *AutoScaling) endpoint() string {
	if a.Endpoint != "" {
		return a.Endpoint
	}
	return "https://autoscaling." + a.Region + ".amazonaws.com"
}

func (a *AutoScaling) client() queryClient {
	return queryClient{endpoint: a.endpoint(), region: a.Region, service: "autoscaling", version: "2011-01-01", creds: a.Credentials, http: a.HTTP}
}

// CompleteLifecycleAction lets the VM carry on (CONTINUE) or discards it (ABANDON).
func (a *AutoScaling) CompleteLifecycleAction(ctx context.Context, group, hook, instance, result string) error {
	return a.client().call(ctx, "CompleteLifecycleAction", url.Values{
		"AutoScalingGroupName":  {group},
		"LifecycleHookName":     {hook},
		"InstanceId":            {instance},
		"LifecycleActionResult": {result},
	}, nil)
}

// RecordLifecycleActionHeartbeat restarts the hook's timeout.
func (a *AutoScaling) RecordLifecycleActionHeartbeat(ctx context.Context, group, hook, instance string) error {
	return a.client().call(ctx, "RecordLifecycleActionHeartbeat", url.Values{
		"AutoScalingGroupName": {group},
		"LifecycleHookName":    {hook},
		"InstanceId":           {instance},
	}, nil)
}

// STS takes on component roles.
type STS struct {
	Endpoint    string // It defaults to https://sts.<region>.amazonaws.com.
	Region      string
	Credentials Provider
	HTTP        *http.Client
}

func (s *STS) endpoint() string {
	if s.Endpoint != "" {
		return s.Endpoint
	}
	return "https://sts." + s.Region + ".amazonaws.com"
}

var sessionNameUnsafe = regexp.MustCompile(`[^\w+=,.@-]`)

// AssumeRole returns credentials for role. The session name shows in CloudTrail; it is
// made safe for STS, which allows 64 characters of [\w+=,.@-].
func (s *STS) AssumeRole(ctx context.Context, role, session string, duration time.Duration) (Credentials, error) {
	session = sessionNameUnsafe.ReplaceAllString(session, "-")
	if len(session) > 64 {
		session = session[:64]
	}
	if len(session) < 2 {
		session = "protostrap"
	}
	var out struct {
		Credentials struct {
			AccessKeyID     string    `xml:"AccessKeyId"`
			SecretAccessKey string    `xml:"SecretAccessKey"`
			SessionToken    string    `xml:"SessionToken"`
			Expiration      time.Time `xml:"Expiration"`
		} `xml:"AssumeRoleResult>Credentials"`
	}
	q := queryClient{endpoint: s.endpoint(), region: s.Region, service: "sts", version: "2011-06-15", creds: s.Credentials, http: s.HTTP}
	err := q.call(ctx, "AssumeRole", url.Values{
		"RoleArn":         {role},
		"RoleSessionName": {session},
		"DurationSeconds": {fmt.Sprint(int(duration.Seconds()))},
	}, &out)
	if err != nil {
		return Credentials{}, err
	}
	c := out.Credentials
	return Credentials{AccessKeyID: c.AccessKeyID, SecretAccessKey: c.SecretAccessKey, SessionToken: c.SessionToken, Expires: c.Expiration}, nil
}

// EC2 is what an ingress VM needs to take over its zone.
type EC2 struct {
	Endpoint    string // It defaults to https://ec2.<region>.amazonaws.com.
	Region      string
	Credentials Provider
	HTTP        *http.Client
}

func (e *EC2) endpoint() string {
	if e.Endpoint != "" {
		return e.Endpoint
	}
	return "https://ec2." + e.Region + ".amazonaws.com"
}

func (e *EC2) client() queryClient {
	return queryClient{endpoint: e.endpoint(), region: e.Region, service: "ec2", version: "2016-11-15", creds: e.Credentials, http: e.HTTP}
}

// AddressHolder is the instance an Elastic IP is associated with, or "".
func (e *EC2) AddressHolder(ctx context.Context, allocationID string) (string, error) {
	var out struct {
		Items []struct {
			InstanceID string `xml:"instanceId"`
		} `xml:"addressesSet>item"`
	}
	if err := e.client().call(ctx, "DescribeAddresses", url.Values{"AllocationId.1": {allocationID}}, &out); err != nil {
		return "", err
	}
	if len(out.Items) == 0 {
		return "", fmt.Errorf("ec2: no address %s", allocationID)
	}
	return out.Items[0].InstanceID, nil
}

// Instance returns when an instance launched and its state, which is "terminated" if the
// instance is gone.
func (e *EC2) Instance(ctx context.Context, id string) (time.Time, string, error) {
	var out struct {
		Instances []struct {
			LaunchTime time.Time `xml:"launchTime"`
			State      string    `xml:"instanceState>name"`
		} `xml:"reservationSet>item>instancesSet>item"`
	}
	err := e.client().call(ctx, "DescribeInstances", url.Values{"InstanceId.1": {id}}, &out)
	if errorCode(err) == "InvalidInstanceID.NotFound" {
		return time.Time{}, "terminated", nil
	}
	if err != nil {
		return time.Time{}, "", err
	}
	if len(out.Instances) == 0 {
		return time.Time{}, "terminated", nil
	}
	return out.Instances[0].LaunchTime, out.Instances[0].State, nil
}

// AssociateAddress moves an Elastic IP to an instance.
func (e *EC2) AssociateAddress(ctx context.Context, allocationID, instanceID string) error {
	return e.client().call(ctx, "AssociateAddress", url.Values{
		"AllocationId": {allocationID}, "InstanceId": {instanceID}, "AllowReassociation": {"true"},
	}, nil)
}

// RouteVia points a route table's destination at an instance, creating the route if it
// doesn't exist.
func (e *EC2) RouteVia(ctx context.Context, routeTable, cidr, instanceID string) error {
	params := url.Values{"RouteTableId": {routeTable}, "DestinationCidrBlock": {cidr}, "InstanceId": {instanceID}}
	err := e.client().call(ctx, "ReplaceRoute", params, nil)
	if errorCode(err) == "InvalidRoute.NotFound" {
		return e.client().call(ctx, "CreateRoute", params, nil)
	}
	return err
}

// DisableSourceDestCheck lets an instance forward traffic that isn't its own, for NAT.
func (e *EC2) DisableSourceDestCheck(ctx context.Context, instanceID string) error {
	return e.client().call(ctx, "ModifyInstanceAttribute", url.Values{
		"InstanceId": {instanceID}, "SourceDestCheck.Value": {"false"},
	}, nil)
}
