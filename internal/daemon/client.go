package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	kitdaemon "go.kenn.io/kit/daemon"
	"go.kenn.io/kwt/service"
)

type Client struct {
	endpoint kitdaemon.Endpoint
	token    string
	http     *http.Client
}

func NewVerifiedClient(
	ctx context.Context,
	rec kitdaemon.RuntimeRecord,
	token string,
) (*Client, error) {
	if token == "" {
		return nil, errors.New("daemon bearer token is empty")
	}
	proof, err := kitdaemon.NewProof([]byte(token))
	if err != nil {
		return nil, err
	}
	info, err := proof.Probe(ctx, rec, kitdaemon.ProbeOptions{
		ExpectedService: ServiceName,
		Timeout:         time.Second,
	})
	if err != nil {
		return nil, fmt.Errorf("prove kwt daemon endpoint: %w", err)
	}
	if info.PID != rec.PID {
		return nil, errors.New("daemon PID does not match runtime record")
	}
	ep := rec.Endpoint()
	return &Client{
		endpoint: ep,
		token:    token,
		http: ep.HTTPClient(kitdaemon.HTTPClientOptions{
			Timeout:               5 * time.Second,
			ResponseHeaderTimeout: 2 * time.Second,
		}),
	}, nil
}

func newClient(ep kitdaemon.Endpoint, token string, client *http.Client) *Client {
	return &Client{endpoint: ep, token: token, http: client}
}

func (c *Client) Status(ctx context.Context) (Status, error) {
	var status Status
	err := c.do(ctx, http.MethodGet, "/api/v1/status", nil, &status)
	return status, err
}

func (c *Client) Shutdown(
	ctx context.Context,
	reason string,
) (ShutdownResponse, error) {
	var output ShutdownResponse
	err := c.do(
		ctx,
		http.MethodPost,
		"/api/v1/daemon/shutdown",
		ShutdownRequest{Reason: reason},
		&output,
	)
	return output, err
}

func (c *Client) do(
	ctx context.Context,
	method string,
	path string,
	input any,
	output any,
) error {
	var body io.Reader
	if input != nil {
		encoded, err := json.Marshal(input)
		if err != nil {
			return err
		}
		body = bytes.NewReader(encoded)
	}
	req, err := http.NewRequestWithContext(
		ctx,
		method,
		c.endpoint.BaseURL()+path,
		body,
	)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if input != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return service.NewError(
			service.TransportFailure,
			"kwt daemon request failed",
			true,
			nil,
			err,
		)
	}
	defer func() { _ = resp.Body.Close() }()
	limited := io.LimitReader(resp.Body, 1<<20)
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return decodeProblem(resp.StatusCode, limited)
	}
	if output == nil {
		return nil
	}
	if err := json.NewDecoder(limited).Decode(output); err != nil {
		return service.NewError(
			service.TransportFailure,
			"decode kwt daemon response",
			true,
			nil,
			err,
		)
	}
	return nil
}

func decodeProblem(status int, body io.Reader) error {
	var problem Problem
	if err := json.NewDecoder(body).Decode(&problem); err != nil {
		return service.NewError(
			service.TransportFailure,
			fmt.Sprintf("kwt daemon returned HTTP %d", status),
			true,
			nil,
			err,
		)
	}
	code, ok := problemCode(status, problem.Code)
	if !ok {
		return service.NewError(
			service.TransportFailure,
			fmt.Sprintf("kwt daemon returned HTTP %d", status),
			true,
			nil,
			nil,
		)
	}
	var details map[string]any
	if problem.DrainDeadline != nil {
		details = map[string]any{"drain_deadline": *problem.DrainDeadline}
	}
	message := problem.Detail
	if message == "" {
		message = http.StatusText(status)
	}
	return service.NewError(code, message, problem.Retryable, details, nil)
}

func problemCode(status int, code string) (service.Code, bool) {
	switch code {
	case "daemon_draining":
		return service.Busy, true
	case string(service.InvalidRequest):
		return service.InvalidRequest, true
	case string(service.PermissionDenied):
		return service.PermissionDenied, true
	case string(service.NotFound):
		return service.NotFound, true
	case string(service.Conflict):
		return service.Conflict, true
	case string(service.Busy):
		return service.Busy, true
	case string(service.InteractionRequired):
		return service.InteractionRequired, true
	case string(service.ConnectionChanged):
		return service.ConnectionChanged, true
	case string(service.Unsupported):
		return service.Unsupported, true
	case string(service.TransportFailure):
		return service.TransportFailure, true
	case string(service.Internal):
		return service.Internal, true
	}
	switch status {
	case http.StatusBadRequest:
		return service.InvalidRequest, true
	case http.StatusUnauthorized, http.StatusForbidden:
		return service.PermissionDenied, true
	case http.StatusNotFound:
		return service.NotFound, true
	case http.StatusConflict:
		return service.Conflict, true
	case http.StatusServiceUnavailable:
		return service.Busy, true
	default:
		return service.TransportFailure, false
	}
}
