// Package httproutes adapts a stdlib http.Handler to the SDK's HttpRoutes.v1
// gRPC service.
package httproutes

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync/atomic"

	pluginv1 "github.com/ContinuumApp/continuum-plugin-sdk/pkg/pluginproto/continuum/plugin/v1"
)

type Server struct {
	pluginv1.UnimplementedHttpRoutesServer
	handler atomic.Pointer[http.Handler]
}

func NewServer() *Server { return &Server{} }

func (s *Server) SetHandler(h http.Handler) {
	if h == nil {
		s.handler.Store(nil)
		return
	}
	s.handler.Store(&h)
}

func (s *Server) Handle(_ context.Context, req *pluginv1.HandleHTTPRequest) (*pluginv1.HandleHTTPResponse, error) {
	hPtr := s.handler.Load()
	if hPtr == nil {
		return &pluginv1.HandleHTTPResponse{
			StatusCode: http.StatusServiceUnavailable,
			Body:       []byte(`{"error":{"code":"not_ready","message":"plugin not configured"}}`),
			Headers:    map[string]string{"Content-Type": "application/json"},
		}, nil
	}
	h := *hPtr

	rawQuery := ""
	if req.GetQuery() != nil {
		vals := url.Values{}
		for k, v := range req.GetQuery().GetFields() {
			// Use the scalar value, not v.String() (which is the protobuf
			// debug form: a number arrives as "number_value:50", corrupting
			// ?limit= / ?library_id= so pagination/scoping silently breaks).
			switch val := v.AsInterface().(type) {
			case string:
				vals.Set(k, val)
			case bool:
				vals.Set(k, strconv.FormatBool(val))
			case float64:
				vals.Set(k, strconv.FormatFloat(val, 'f', -1, 64))
			}
		}
		rawQuery = vals.Encode()
	}

	u := &url.URL{Path: req.GetPath(), RawQuery: rawQuery}
	method := req.GetMethod()
	if method == "" {
		method = http.MethodGet
	}
	httpReq := httptest.NewRequest(method, u.String(), bytes.NewReader(req.GetBody()))
	for k, v := range req.GetHeaders() {
		httpReq.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httpReq)
	body, _ := io.ReadAll(rec.Result().Body)
	headers := map[string]string{}
	for k, vs := range rec.Header() {
		if len(vs) > 0 {
			headers[k] = vs[0]
		}
	}
	return &pluginv1.HandleHTTPResponse{
		StatusCode: int32(rec.Code),
		Headers:    headers,
		Body:       body,
	}, nil
}
