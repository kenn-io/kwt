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
	publicworktree "go.kenn.io/kwt/worktree"
)

type Client struct {
	endpoint      kitdaemon.Endpoint
	token         string
	controlHTTP   *http.Client
	inventoryHTTP *http.Client
}

var ErrResponseTooLarge = errors.New("kwt daemon response is too large")

const (
	controlRequestTimeout           = 2 * time.Second
	inventoryResponseHeadroom       = 5 * time.Second
	inventoryRequestTimeout         = publicworktree.DefaultRefreshTimeout + inventoryResponseHeadroom
	controlResponseLimit      int64 = 1 << 20
	inventoryResponseLimit          = 64 << 20
)

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
		controlHTTP: ep.HTTPClient(kitdaemon.HTTPClientOptions{
			Timeout:               controlRequestTimeout,
			ResponseHeaderTimeout: controlRequestTimeout,
		}),
		inventoryHTTP: ep.HTTPClient(kitdaemon.HTTPClientOptions{
			Timeout:               inventoryRequestTimeout,
			ResponseHeaderTimeout: inventoryRequestTimeout,
		}),
	}, nil
}

func (c *Client) Inventory(ctx context.Context, request publicworktree.Request) (publicworktree.Result, error) {
	expansion, err := publicworktree.CaptureExpansionContext()
	if err != nil {
		return publicworktree.Result{}, err
	}
	request.Expansion = expansion
	var result publicworktree.Result
	err = c.doWith(
		ctx,
		c.inventoryHTTP,
		inventoryResponseLimit,
		http.MethodPost,
		"/api/v1/inventory",
		request,
		&result,
	)
	return result, err
}

func (c *Client) ApproveConfig(ctx context.Context, approval publicworktree.ConfigApproval) error {
	return c.do(ctx, http.MethodPost, "/api/v1/config/trust", approval, nil)
}

func newClient(ep kitdaemon.Endpoint, token string, client *http.Client) *Client {
	return &Client{
		endpoint: ep, token: token, controlHTTP: client, inventoryHTTP: client,
	}
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
	return c.doWith(
		ctx,
		c.controlHTTP,
		controlResponseLimit,
		method,
		path,
		input,
		output,
	)
}

func (c *Client) doWith(
	ctx context.Context,
	client *http.Client,
	responseLimit int64,
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
	resp, err := client.Do(req)
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
	encoded, err := readResponse(resp.Body, responseLimit, path)
	if err != nil {
		message := "read kwt daemon response"
		if errors.Is(err, ErrResponseTooLarge) {
			message = err.Error()
		}
		return service.NewError(
			service.TransportFailure,
			message,
			true,
			nil,
			err,
		)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return decodeProblem(resp.StatusCode, bytes.NewReader(encoded))
	}
	if output == nil {
		return nil
	}
	if err := json.Unmarshal(encoded, output); err != nil {
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

func readResponse(body io.Reader, limit int64, path string) ([]byte, error) {
	encoded, err := io.ReadAll(io.LimitReader(body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(encoded)) > limit {
		return nil, fmt.Errorf(
			"%w: %s exceeds %d bytes",
			ErrResponseTooLarge,
			path,
			limit,
		)
	}
	return encoded, nil
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
	details := problem.Details
	if problem.DrainDeadline != nil {
		if details == nil {
			details = make(map[string]any)
		}
		details["drain_deadline"] = *problem.DrainDeadline
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
